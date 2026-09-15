package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/protocol"
	"golang.org/x/sys/unix"
)

var (
	ErrSocketInUse  = errors.New("clipboard socket is already in use")
	ErrSocketOwner  = errors.New("clipboard socket is owned by another user")
	ErrServerClosed = errors.New("clipboard server is closed")
)

const (
	socketDirectory = "sysc-clipboard"
	socketName      = "control.v1.sock"
	clientQueueSize = 8
	clientTimeout   = 5 * time.Second
)

// DefaultSocketPath resolves the private per-user control socket.
func DefaultSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is not an absolute path")
	}
	return filepath.Join(runtimeDir, socketDirectory, socketName), nil
}

// Server serves one private Unix socket for one reducer service.
type Server struct {
	service    *Service
	socketPath string

	mu          sync.Mutex
	listener    *net.UnixListener
	connections map[*net.UnixConn]struct{}
	closed      bool
}

func NewServer(service *Service, socketPath string) (*Server, error) {
	if service == nil {
		return nil, errors.New("clipboard server requires a service")
	}
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, errors.New("clipboard socket path must be absolute")
	}
	return &Server{service: service, socketPath: socketPath, connections: make(map[*net.UnixConn]struct{})}, nil
}

// Serve listens until the context is cancelled or the listener fails.
func (server *Server) Serve(ctx context.Context) error {
	listener, err := listenPrivate(server.socketPath)
	if err != nil {
		return err
	}
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		_ = listener.Close()
		_ = os.Remove(server.socketPath)
		return ErrServerClosed
	}
	server.listener = listener
	server.mu.Unlock()

	stopListener := make(chan struct{})
	defer close(stopListener)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stopListener:
		}
	}()
	defer func() {
		_ = listener.Close()
		server.mu.Lock()
		if server.listener == listener {
			server.listener = nil
		}
		server.mu.Unlock()
		_ = os.Remove(server.socketPath)
	}()

	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			server.mu.Lock()
			closed := server.closed
			server.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept clipboard client: %w", err)
		}
		uid, err := peerUID(connection)
		if err != nil || uid != uint32(os.Getuid()) {
			_ = connection.Close()
			continue
		}
		server.addConnection(connection)
		go server.serveClient(ctx, connection)
	}
}

// Close stops listening and disconnects clients. It is safe to call more than
// once.
func (server *Server) Close() error {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return nil
	}
	server.closed = true
	listener := server.listener
	for connection := range server.connections {
		_ = connection.Close()
	}
	server.connections = make(map[*net.UnixConn]struct{})
	server.mu.Unlock()
	if listener != nil {
		return listener.Close()
	}
	return nil
}

func (server *Server) addConnection(connection *net.UnixConn) {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		_ = connection.Close()
		return
	}
	server.connections[connection] = struct{}{}
	server.mu.Unlock()
}

func (server *Server) removeConnection(connection *net.UnixConn) {
	server.mu.Lock()
	delete(server.connections, connection)
	server.mu.Unlock()
}

func (server *Server) serveClient(parent context.Context, connection *net.UnixConn) {
	defer server.removeConnection(connection)
	defer connection.Close()

	if err := connection.SetReadDeadline(time.Now().Add(clientTimeout)); err != nil {
		return
	}
	hello, err := protocol.ReadMessage(connection)
	if err != nil || hello.Type != protocol.TypeHello {
		return
	}
	subscription, err := server.service.Subscribe(parent)
	if err != nil {
		return
	}
	defer subscription.Close()
	initial, ok := nextSubscriptionMessage(subscription.Updates)
	if !ok || initial.Type != protocol.TypeSnapshot || initial.Snapshot == nil {
		return
	}
	tracker := newRevisionTracker(initial.Snapshot.Revision)
	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		return
	}
	if err := writeClientMessage(connection, server.helloMessage(*initial.Snapshot)); err != nil {
		return
	}
	if err := writeClientMessage(connection, initial); err != nil {
		return
	}

	clientContext, cancel := context.WithCancel(parent)
	defer cancel()
	queue := newOutboundQueue(clientQueueSize)
	defer queue.Close()

	var workers sync.WaitGroup
	clientDone := make(chan struct{})
	var doneOnce sync.Once
	finish := func() {
		doneOnce.Do(func() {
			close(clientDone)
			cancel()
			_ = connection.Close()
			queue.Close()
		})
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			message, ok := queue.Next(clientContext)
			if !ok {
				return
			}
			if err := writeClientMessage(connection, message); err != nil {
				finish()
				return
			}
		}
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case message, ok := <-subscription.Updates:
				if !ok {
					finish()
					return
				}
				if err := queue.Enqueue(message, func() (protocol.Message, error) {
					snapshot, err := server.service.CurrentSnapshot(clientContext)
					if err != nil {
						return protocol.Message{}, err
					}
					return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}, nil
				}); err != nil {
					finish()
					return
				}
				tracker.Mark(messageRevision(message))
			case <-clientContext.Done():
				return
			}
		}
	}()

	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			message, err := protocol.ReadMessage(connection)
			if err != nil {
				finish()
				return
			}
			response := server.service.Execute(clientContext, message)
			if response.Type == protocol.TypeAck && response.Ack != nil && !tracker.Wait(clientContext, response.Ack.Revision) {
				finish()
				return
			}
			if err := queue.Enqueue(response, func() (protocol.Message, error) {
				snapshot, err := server.service.CurrentSnapshot(clientContext)
				if err != nil {
					return protocol.Message{}, err
				}
				return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}, nil
			}); err != nil {
				finish()
				return
			}
		}
	}()

	select {
	case <-clientDone:
	case <-parent.Done():
		finish()
	}
	workers.Wait()
}

func messageRevision(message protocol.Message) uint64 {
	switch message.Type {
	case protocol.TypeSnapshot:
		if message.Snapshot != nil {
			return message.Snapshot.Revision
		}
	case protocol.TypeDelta:
		if message.Delta != nil {
			return message.Delta.Revision
		}
	}
	return 0
}

type revisionTracker struct {
	mu       sync.Mutex
	revision uint64
	changed  chan struct{}
}

func newRevisionTracker(revision uint64) *revisionTracker {
	return &revisionTracker{revision: revision, changed: make(chan struct{})}
}

func (tracker *revisionTracker) Mark(revision uint64) {
	tracker.mu.Lock()
	if revision > tracker.revision {
		tracker.revision = revision
		close(tracker.changed)
		tracker.changed = make(chan struct{})
	}
	tracker.mu.Unlock()
}

func (tracker *revisionTracker) Wait(ctx context.Context, revision uint64) bool {
	for {
		tracker.mu.Lock()
		if tracker.revision >= revision {
			tracker.mu.Unlock()
			return true
		}
		changed := tracker.changed
		tracker.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

func (server *Server) helloMessage(snapshot protocol.Snapshot) protocol.Message {
	return protocol.Message{
		Version: protocol.Version,
		Type:    protocol.TypeHello,
		Hello: &protocol.Hello{
			Capabilities: []string{"restore", "pin", "delete", "clear", "thumbnail", "resync"},
			Persistence:  snapshot.Persistence,
			Wayland:      snapshot.Wayland,
		},
	}
}

func nextSubscriptionMessage(updates <-chan protocol.Message) (protocol.Message, bool) {
	message, ok := <-updates
	return message, ok
}

func listenPrivate(path string) (*net.UnixListener, error) {
	if err := ensureSocketDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, ErrSocketInUse
		}
		return nil, fmt.Errorf("listen clipboard socket: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protect clipboard socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		_ = listener.Close()
		_ = os.Remove(path)
		if err != nil {
			return nil, fmt.Errorf("inspect clipboard socket: %w", err)
		}
		return nil, ErrSocketOwner
	}
	return listener, nil
}

func ensureSocketDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return fmt.Errorf("create clipboard socket directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect clipboard socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return fmt.Errorf("clipboard socket directory must be private")
	}
	if !ownedByCurrentUser(info) {
		return ErrSocketOwner
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing clipboard socket: %w", err)
	}
	if !ownedByCurrentUser(info) {
		return ErrSocketOwner
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("existing clipboard socket path is not a socket")
	}
	probe, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
	if dialErr == nil {
		_ = probe.Close()
		return ErrSocketInUse
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("probe existing clipboard socket: %w", dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale clipboard socket: %w", err)
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func peerUID(connection *net.UnixConn) (uint32, error) {
	raw, err := connection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var controlErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = credentials.Uid
	}); err != nil {
		return 0, err
	}
	if controlErr != nil {
		return 0, controlErr
	}
	return uid, nil
}

func writeClientMessage(connection *net.UnixConn, message protocol.Message) error {
	if err := connection.SetWriteDeadline(time.Now().Add(clientTimeout)); err != nil {
		return err
	}
	err := protocol.WriteMessage(connection, message)
	_ = connection.SetWriteDeadline(time.Time{})
	return err
}

type outboundQueue struct {
	mu        sync.Mutex
	pending   []protocol.Message
	capacity  int
	wake      chan struct{}
	done      chan struct{}
	closed    bool
	resyncing bool
}

func newOutboundQueue(capacity int) *outboundQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &outboundQueue{
		capacity: capacity,
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
}

func (queue *outboundQueue) Enqueue(message protocol.Message, currentSnapshot func() (protocol.Message, error)) error {
	queue.mu.Lock()
	if queue.closed {
		queue.mu.Unlock()
		return ErrServerClosed
	}
	if len(queue.pending) < queue.capacity {
		queue.pending = append(queue.pending, message)
		queue.signalLocked()
		queue.mu.Unlock()
		return nil
	}
	if queue.resyncing {
		queue.mu.Unlock()
		return nil
	}
	queue.resyncing = true
	queue.pending = nil
	queue.mu.Unlock()

	snapshot, err := currentSnapshot()
	queue.mu.Lock()
	queue.resyncing = false
	defer queue.mu.Unlock()
	if queue.closed {
		return ErrServerClosed
	}
	if err != nil {
		return err
	}
	queue.pending = append(queue.pending, snapshot)
	queue.signalLocked()
	return nil
}

func (queue *outboundQueue) Next(ctx context.Context) (protocol.Message, bool) {
	for {
		queue.mu.Lock()
		if queue.closed {
			queue.mu.Unlock()
			return protocol.Message{}, false
		}
		if len(queue.pending) > 0 {
			message := queue.pending[0]
			queue.pending = queue.pending[1:]
			queue.mu.Unlock()
			return message, true
		}
		queue.mu.Unlock()
		select {
		case <-queue.wake:
		case <-queue.done:
			return protocol.Message{}, false
		case <-ctx.Done():
			return protocol.Message{}, false
		}
	}
}

func (queue *outboundQueue) TryNext() (protocol.Message, bool) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed || len(queue.pending) == 0 {
		return protocol.Message{}, false
	}
	message := queue.pending[0]
	queue.pending = queue.pending[1:]
	return message, true
}

func (queue *outboundQueue) Close() {
	queue.mu.Lock()
	if !queue.closed {
		queue.closed = true
		close(queue.done)
	}
	queue.mu.Unlock()
}

func (queue *outboundQueue) signalLocked() {
	select {
	case queue.wake <- struct{}{}:
	default:
	}
}

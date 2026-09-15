// Package client is the metadata-only sysc-clipboard daemon client.
package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

var (
	ErrClientClosed     = errors.New("clipboard client is closed")
	ErrClientRunning    = errors.New("clipboard client is already running")
	ErrCommandQueueFull = errors.New("clipboard client command queue is full")
	ErrRevisionGap      = errors.New("clipboard client revision gap")
)

const (
	maxCommandQueue  = 8
	inboundQueueSize = 8
)

// Update is the current metadata snapshot plus the protocol message that
// caused it. Payloads are never present; thumbnail bytes are response data and
// are copied before publication.
type Update struct {
	Message   protocol.Message
	Snapshot  protocol.Snapshot
	Connected bool
}

type clientOptions struct {
	dial dialFunc
	wait waitFunc
}

// Client maintains one reconnecting connection to the daemon. Command
// methods only validate and enqueue; responses arrive through Updates.
type Client struct {
	socketPath string
	dial       dialFunc
	wait       waitFunc
	commands   chan protocol.Message
	updates    chan Update
	done       chan struct{}
	runOnce    sync.Once

	mu        sync.RWMutex
	snapshot  protocol.Snapshot
	connected bool
	closed    bool
	published bool
	requestID uint64
}

// New creates a client for an absolute daemon socket path.
func New(socketPath string) (*Client, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, errors.New("clipboard client socket path must be absolute")
	}
	return newClientWithOptions(socketPath, clientOptions{}), nil
}

func newClientWithOptions(socketPath string, options clientOptions) *Client {
	if options.dial == nil {
		options.dial = dialSocket
	}
	if options.wait == nil {
		options.wait = waitReconnect
	}
	return &Client{
		socketPath: socketPath,
		dial:       options.dial,
		wait:       options.wait,
		commands:   make(chan protocol.Message, maxCommandQueue),
		updates:    make(chan Update, inboundQueueSize),
		done:       make(chan struct{}),
		snapshot: protocol.Snapshot{
			Persistence: protocol.PersistenceUnavailable,
			Wayland:     protocol.WaylandUnavailable,
		},
	}
}

// Updates returns a bounded stream of current metadata and command responses.
func (client *Client) Updates() <-chan Update { return client.updates }

// Current returns a copied snapshot and the last known connection state.
func (client *Client) Current() (protocol.Snapshot, bool) {
	client.mu.RLock()
	defer client.mu.RUnlock()
	return cloneSnapshot(client.snapshot), client.connected
}

// Run connects, applies the handshake snapshot, and reconnects until ctx is
// cancelled or the injected transport returns an error.
func (client *Client) Run(ctx context.Context) error {
	first := false
	client.runOnce.Do(func() { first = true })
	if !first {
		return ErrClientRunning
	}
	defer func() {
		client.mu.Lock()
		client.closed = true
		client.mu.Unlock()
		close(client.done)
		close(client.updates)
	}()

	delay := minReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connection, err := client.dial(ctx, client.socketPath)
		if err != nil {
			client.markUnavailable()
			if err := client.wait(ctx, delay); err != nil {
				return err
			}
			delay = nextReconnectDelay(delay)
			continue
		}

		snapshot, err := client.handshake(ctx, connection)
		if err != nil {
			_ = connection.Close()
			client.markUnavailable()
			if err := client.wait(ctx, delay); err != nil {
				return err
			}
			delay = nextReconnectDelay(delay)
			continue
		}
		delay = minReconnectDelay
		client.markConnected(protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}, snapshot)
		_ = client.serveConnection(ctx, connection)
		_ = connection.Close()
		client.markUnavailable()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := client.wait(ctx, delay); err != nil {
			return err
		}
		delay = nextReconnectDelay(delay)
	}
}

func (client *Client) handshake(ctx context.Context, connection net.Conn) (protocol.Snapshot, error) {
	stopWatcher := watchConnectionContext(ctx, connection)
	defer stopWatcher()
	if err := writeMessage(connection, protocol.Message{
		Version: protocol.Version,
		Type:    protocol.TypeHello,
		Hello:   &protocol.Hello{Capabilities: []string{"restore", "pin", "delete", "clear", "thumbnail", "resync"}},
	}); err != nil {
		return protocol.Snapshot{}, err
	}
	deadline := time.Now().Add(clientTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return protocol.Snapshot{}, err
	}
	defer connection.SetReadDeadline(time.Time{})
	hello, err := protocol.ReadMessage(connection)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	if hello.Type != protocol.TypeHello || hello.Hello == nil {
		return protocol.Snapshot{}, errors.New("clipboard daemon sent an invalid hello")
	}
	snapshotMessage, err := protocol.ReadMessage(connection)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	if snapshotMessage.Type != protocol.TypeSnapshot || snapshotMessage.Snapshot == nil {
		return protocol.Snapshot{}, errors.New("clipboard daemon sent no startup snapshot")
	}
	return cloneSnapshot(*snapshotMessage.Snapshot), nil
}

type inboundMessage struct {
	message protocol.Message
	err     error
}

func (client *Client) serveConnection(ctx context.Context, connection net.Conn) error {
	stopWatcher := watchConnectionContext(ctx, connection)
	defer stopWatcher()
	incoming := make(chan inboundMessage, inboundQueueSize)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			message, err := protocol.ReadMessage(connection)
			if err != nil {
				select {
				case incoming <- inboundMessage{err: err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case incoming <- inboundMessage{message: message}:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		_ = connection.Close()
		<-readerDone
	}()

	resyncPending := false
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case command := <-client.commands:
			if err := writeMessage(connection, command); err != nil {
				return err
			}
		case event := <-incoming:
			if event.err != nil {
				return event.err
			}
			if err := client.handleMessage(connection, event.message, &resyncPending); err != nil {
				return err
			}
		}
	}
}

func (client *Client) handleMessage(connection net.Conn, message protocol.Message, resyncPending *bool) error {
	switch message.Type {
	case protocol.TypeSnapshot:
		if message.Snapshot == nil {
			return errors.New("clipboard snapshot has no body")
		}
		snapshot := cloneSnapshot(*message.Snapshot)
		client.markConnected(message, snapshot)
		*resyncPending = false
		return nil
	case protocol.TypeDelta:
		if message.Delta == nil {
			return errors.New("clipboard delta has no body")
		}
		if *resyncPending {
			return nil
		}
		current, _ := client.Current()
		next, err := applyDelta(current, *message.Delta)
		if errors.Is(err, ErrRevisionGap) {
			*resyncPending = true
			return client.requestResync(connection)
		}
		if err != nil {
			return err
		}
		client.markConnected(message, next)
		return nil
	case protocol.TypeAck, protocol.TypeError, protocol.TypeThumbnail:
		client.emit(message, true)
		return nil
	default:
		return fmt.Errorf("unexpected clipboard server message %q", message.Type)
	}
}

func (client *Client) requestResync(connection net.Conn) error {
	message := client.newCommand(protocol.TypeResync, "", nil, "", 0)
	return writeMessage(connection, message)
}

func (client *Client) markConnected(message protocol.Message, snapshot protocol.Snapshot) {
	client.mu.Lock()
	client.snapshot = cloneSnapshot(snapshot)
	client.connected = true
	client.published = true
	client.mu.Unlock()
	client.emit(message, true)
}

func (client *Client) markUnavailable() {
	client.mu.Lock()
	if !client.published || client.connected || client.snapshot.Wayland != protocol.WaylandUnavailable {
		client.snapshot.Wayland = protocol.WaylandUnavailable
		client.connected = false
		client.published = true
		snapshot := cloneSnapshot(client.snapshot)
		client.mu.Unlock()
		message := protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}
		client.emit(message, false)
		return
	}
	client.mu.Unlock()
}

func (client *Client) emit(message protocol.Message, connected bool) {
	client.mu.RLock()
	snapshot := cloneSnapshot(client.snapshot)
	client.mu.RUnlock()
	update := Update{Message: cloneMessage(message), Snapshot: snapshot, Connected: connected}
	select {
	case client.updates <- update:
		return
	default:
	}
	for {
		select {
		case <-client.updates:
			continue
		default:
			break
		}
		break
	}
	select {
	case client.updates <- update:
	default:
	}
}

func (client *Client) enqueue(message protocol.Message) error {
	if err := protocol.ValidateMessage(message); err != nil {
		return err
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return ErrClientClosed
	}
	select {
	case client.commands <- cloneMessage(message):
		return nil
	default:
		return ErrCommandQueueFull
	}
}

func (client *Client) newCommand(messageType protocol.MessageType, id string, pinned *bool, scope protocol.ClearScope, maxPX uint16) protocol.Message {
	client.mu.Lock()
	client.requestID++
	requestID := fmt.Sprintf("request-%d", client.requestID)
	client.mu.Unlock()
	return protocol.Message{
		Version:   protocol.Version,
		Type:      messageType,
		RequestID: requestID,
		ID:        id,
		Pinned:    pinned,
		Scope:     scope,
		MaxPX:     maxPX,
	}
}

func (client *Client) Restore(id string) error {
	return client.enqueue(client.newCommand(protocol.TypeRestore, id, nil, "", 0))
}

func (client *Client) Pin(id string, pinned bool) error {
	return client.enqueue(client.newCommand(protocol.TypePin, id, &pinned, "", 0))
}

func (client *Client) Delete(id string) error {
	return client.enqueue(client.newCommand(protocol.TypeDelete, id, nil, "", 0))
}

func (client *Client) Clear(scope protocol.ClearScope) error {
	return client.enqueue(client.newCommand(protocol.TypeClear, "", nil, scope, 0))
}

func (client *Client) Thumbnail(id string, maxPX uint16) error {
	return client.enqueue(client.newCommand(protocol.TypeThumbnail, id, nil, "", maxPX))
}

func (client *Client) Resync() error {
	return client.enqueue(client.newCommand(protocol.TypeResync, "", nil, "", 0))
}

func writeMessage(connection net.Conn, message protocol.Message) error {
	if err := connection.SetWriteDeadline(time.Now().Add(clientTimeout)); err != nil {
		return err
	}
	err := protocol.WriteMessage(connection, message)
	_ = connection.SetWriteDeadline(time.Time{})
	return err
}

func applyDelta(current protocol.Snapshot, delta protocol.Delta) (protocol.Snapshot, error) {
	if delta.Revision != current.Revision+1 {
		return protocol.Snapshot{}, ErrRevisionGap
	}
	next := cloneSnapshot(current)
	for _, change := range delta.Changes {
		index := entryIndex(next.Entries, change.ID)
		switch change.Kind {
		case protocol.ChangeAdded:
			if index >= 0 || change.Entry == nil {
				return protocol.Snapshot{}, errors.New("clipboard delta adds an existing entry")
			}
			next.Entries = insertEntry(next.Entries, *change.Entry)
		case protocol.ChangeMoved, protocol.ChangeUpdated:
			if index < 0 || change.Entry == nil {
				return protocol.Snapshot{}, errors.New("clipboard delta updates a missing entry")
			}
			next.Entries = append(next.Entries[:index], next.Entries[index+1:]...)
			next.Entries = insertEntry(next.Entries, *change.Entry)
		case protocol.ChangeRemoved:
			if index < 0 {
				return protocol.Snapshot{}, errors.New("clipboard delta removes a missing entry")
			}
			next.Entries = append(next.Entries[:index], next.Entries[index+1:]...)
		default:
			return protocol.Snapshot{}, errors.New("clipboard delta has an unknown change")
		}
	}
	next.Revision = delta.Revision
	if delta.Persistence != "" {
		next.Persistence = delta.Persistence
	}
	if delta.Wayland != "" {
		next.Wayland = delta.Wayland
	}
	if err := protocol.ValidateSnapshot(next); err != nil {
		return protocol.Snapshot{}, err
	}
	return next, nil
}

func entryIndex(entries []protocol.Entry, id string) int {
	for index, entry := range entries {
		if entry.ID == id {
			return index
		}
	}
	return -1
}

func insertEntry(entries []protocol.Entry, entry protocol.Entry) []protocol.Entry {
	index := 0
	if !entry.Pinned {
		for index < len(entries) && entries[index].Pinned {
			index++
		}
	}
	entries = append(entries, protocol.Entry{})
	copy(entries[index+1:], entries[index:])
	entries[index] = cloneEntry(entry)
	return entries
}

func cloneSnapshot(snapshot protocol.Snapshot) protocol.Snapshot {
	cloned := snapshot
	cloned.Entries = make([]protocol.Entry, len(snapshot.Entries))
	for index, entry := range snapshot.Entries {
		cloned.Entries[index] = cloneEntry(entry)
	}
	return cloned
}

func cloneEntry(entry protocol.Entry) protocol.Entry {
	entry.OfferedMIME = append([]string(nil), entry.OfferedMIME...)
	return entry
}

func cloneMessage(message protocol.Message) protocol.Message {
	cloned := message
	if message.Pinned != nil {
		pinned := *message.Pinned
		cloned.Pinned = &pinned
	}
	if message.Hello != nil {
		hello := *message.Hello
		hello.Capabilities = append([]string(nil), message.Hello.Capabilities...)
		cloned.Hello = &hello
	}
	if message.Snapshot != nil {
		snapshot := cloneSnapshot(*message.Snapshot)
		cloned.Snapshot = &snapshot
	}
	if message.Delta != nil {
		delta := *message.Delta
		delta.Changes = make([]protocol.Change, len(message.Delta.Changes))
		for index, change := range message.Delta.Changes {
			delta.Changes[index] = change
			if change.Entry != nil {
				entry := cloneEntry(*change.Entry)
				delta.Changes[index].Entry = &entry
			}
		}
		cloned.Delta = &delta
	}
	if message.Ack != nil {
		ack := *message.Ack
		cloned.Ack = &ack
	}
	if message.Error != nil {
		errorBody := *message.Error
		cloned.Error = &errorBody
	}
	if message.Thumbnail != nil {
		thumbnail := *message.Thumbnail
		thumbnail.Data = append([]byte(nil), message.Thumbnail.Data...)
		cloned.Thumbnail = &thumbnail
	}
	return cloned
}

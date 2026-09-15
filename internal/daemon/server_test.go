package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
	"golang.org/x/sys/unix"
)

func TestServerPrivateSocketHandshakeAndSnapshot(t *testing.T) {
	svc := newTestService(t, history.New(1))
	socketPath := filepath.Join(t.TempDir(), "sysc-clipboard", "control.v1.sock")
	server, err := NewServer(svc, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(ctx) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	waitForSocket(t, socketPath)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	uid, err := peerUID(conn.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("peer UID = %d, want %d", uid, os.Getuid())
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteMessage(conn, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{}}); err != nil {
		t.Fatal(err)
	}
	hello, err := protocol.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Type != protocol.TypeHello || hello.Hello == nil || hello.Hello.Wayland != protocol.WaylandUnavailable {
		t.Fatalf("hello = %+v", hello)
	}
	snapshot, err := protocol.ReadMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Type != protocol.TypeSnapshot || snapshot.Snapshot == nil || snapshot.Snapshot.Revision != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	for _, want := range []struct {
		path string
		mode os.FileMode
	}{
		{path: filepath.Dir(socketPath), mode: 0700},
		{path: socketPath, mode: 0600},
	} {
		info, err := os.Stat(want.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want.mode {
			t.Fatalf("%s mode = %o, want %o", want.path, info.Mode().Perm(), want.mode)
		}
	}
}

func TestServerDisconnectsMalformedInput(t *testing.T) {
	svc := newTestService(t, history.New(1))
	socketPath := filepath.Join(t.TempDir(), "sysc-clipboard", "control.v1.sock")
	server, err := NewServer(svc, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Serve(ctx) }()
	t.Cleanup(func() { _ = server.Close() })
	waitForSocket(t, socketPath)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := protocol.WriteFrame(conn, []byte(`{"version":1,"type":"hello"`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := protocol.ReadFrame(conn); err == nil {
		t.Fatal("malformed client remained connected")
	}
}

func TestServerRejectsSecondServerAndRemovesSameUIDStaleSocket(t *testing.T) {
	svc := newTestService(t, history.New(1))
	socketPath := filepath.Join(t.TempDir(), "sysc-clipboard", "control.v1.sock")
	if err := ensureSocketDirectory(filepath.Dir(socketPath)); err != nil {
		t.Fatal(err)
	}
	activeFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(activeFD, &unix.SockaddrUnix{Name: socketPath}); err != nil {
		_ = unix.Close(activeFD)
		t.Fatal(err)
	}
	if err := unix.Listen(activeFD, 1); err != nil {
		_ = unix.Close(activeFD)
		t.Fatal(err)
	}
	second, err := NewServer(svc, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Serve(context.Background()); !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("second server error = %v, want ErrSocketInUse", err)
	}
	if err := unix.Close(activeFD); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("closed listener did not leave stale socket: %v", err)
	}

	third, err := NewServer(svc, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	thirdCtx, thirdCancel := context.WithCancel(context.Background())
	defer thirdCancel()
	thirdErr := make(chan error, 1)
	go func() { thirdErr <- third.Serve(thirdCtx) }()
	waitForSocket(t, socketPath)
	_ = third.Close()
	select {
	case <-thirdErr:
	case <-time.After(2 * time.Second):
		t.Fatal("third server did not stop")
	}
}

func TestOutboundQueueReplacesOverflowWithOneSnapshot(t *testing.T) {
	queue := newOutboundQueue(2)
	defer queue.Close()
	queue.Enqueue(protocol.Message{Version: protocol.Version, Type: protocol.TypeDelta, Delta: &protocol.Delta{Revision: 1, Changes: []protocol.Change{{Kind: protocol.ChangeRemoved, ID: "one"}}}}, func() (protocol.Message, error) {
		return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &protocol.Snapshot{Revision: 3, Persistence: protocol.PersistenceUnavailable, Wayland: protocol.WaylandUnavailable}}, nil
	})
	queue.Enqueue(protocol.Message{Version: protocol.Version, Type: protocol.TypeDelta, Delta: &protocol.Delta{Revision: 2, Changes: []protocol.Change{{Kind: protocol.ChangeRemoved, ID: "two"}}}}, func() (protocol.Message, error) {
		return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &protocol.Snapshot{Revision: 3, Persistence: protocol.PersistenceUnavailable, Wayland: protocol.WaylandUnavailable}}, nil
	})
	queue.Enqueue(protocol.Message{Version: protocol.Version, Type: protocol.TypeDelta, Delta: &protocol.Delta{Revision: 4, Changes: []protocol.Change{{Kind: protocol.ChangeRemoved, ID: "four"}}}}, func() (protocol.Message, error) {
		return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &protocol.Snapshot{Revision: 4, Persistence: protocol.PersistenceUnavailable, Wayland: protocol.WaylandUnavailable}}, nil
	})
	message := nextQueueMessage(t, queue)
	if message.Type != protocol.TypeSnapshot || message.Snapshot == nil || message.Snapshot.Revision != 4 {
		t.Fatalf("queue message = %+v", message)
	}
	if third, ok := queue.TryNext(); ok {
		t.Fatalf("queue retained overflow message %+v", third)
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear", path)
}

func nextQueueMessage(t *testing.T, queue *outboundQueue) protocol.Message {
	t.Helper()
	message, ok := queue.Next(context.Background())
	if !ok {
		t.Fatal("queue closed")
	}
	return message
}

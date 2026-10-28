package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

func TestClientHandshakesAndPublishesSnapshot(t *testing.T) {
	entry := testEntry("one", []byte("clipboard"))
	snapshot := testSnapshot(7, []protocol.Entry{entry})
	path := startClientServer(t, func(connection net.Conn) {
		hello, err := protocol.ReadMessage(connection)
		if err != nil || hello.Type != protocol.TypeHello {
			return
		}
		copySupported := false
		for _, capability := range hello.Hello.Capabilities {
			copySupported = copySupported || capability == "copy"
		}
		if !copySupported {
			t.Errorf("client hello capabilities = %v, want copy", hello.Hello.Capabilities)
		}
		_ = protocol.WriteMessage(connection, protocol.Message{
			Version: protocol.Version,
			Type:    protocol.TypeHello,
			Hello:   &protocol.Hello{Persistence: protocol.PersistenceDurable, Wayland: protocol.WaylandReady},
		})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot})
	})

	client := newTestClient(t, path)
	cancel, done := startClient(t, client)
	defer cancel()
	update := nextUpdate(t, client.Updates())
	if !update.Connected || update.Message.Type != protocol.TypeSnapshot || update.Snapshot.Revision != 7 {
		t.Fatalf("handshake update = %+v", update)
	}
	if len(update.Snapshot.Entries) != 1 || update.Snapshot.Entries[0].ID != entry.ID {
		t.Fatalf("handshake entries = %+v", update.Snapshot.Entries)
	}
	if got, connected := client.Current(); !connected || got.Revision != 7 || got.Entries[0].ID != entry.ID {
		t.Fatalf("current snapshot = %+v, connected = %v", got, connected)
	}
	cancel()
	<-done
}

func TestClientAppliesOrderedDeltasAtomically(t *testing.T) {
	first := testEntry("one", []byte("one"))
	second := testEntry("two", []byte("two"))
	path := startClientServer(t, func(connection net.Conn) {
		if !readHello(connection) {
			return
		}
		base := testSnapshot(1, []protocol.Entry{first})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{}})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &base})
		_ = protocol.WriteMessage(connection, protocol.Message{
			Version: protocol.Version,
			Type:    protocol.TypeDelta,
			Delta: &protocol.Delta{
				Revision: 2,
				Changes:  []protocol.Change{{Kind: protocol.ChangeAdded, ID: second.ID, Entry: &second}},
			},
		})
	})

	client := newTestClient(t, path)
	cancel, done := startClient(t, client)
	defer cancel()
	initial := nextUpdate(t, client.Updates())
	if initial.Snapshot.Revision != 1 {
		t.Fatalf("initial snapshot = %+v", initial.Snapshot)
	}
	update := nextUpdate(t, client.Updates())
	if !update.Connected || update.Message.Type != protocol.TypeDelta || update.Snapshot.Revision != 2 {
		t.Fatalf("delta update = %+v", update)
	}
	if len(update.Snapshot.Entries) != 2 || update.Snapshot.Entries[0].ID != second.ID || update.Snapshot.Entries[1].ID != first.ID {
		t.Fatalf("delta entries = %+v", update.Snapshot.Entries)
	}
	cancel()
	<-done
}

func TestClientRequestsResyncOnRevisionGap(t *testing.T) {
	path := startClientServer(t, func(connection net.Conn) {
		if !readHello(connection) {
			return
		}
		first := testEntry("one", []byte("one"))
		base := testSnapshot(1, []protocol.Entry{first})
		final := testSnapshot(3, []protocol.Entry{first, testEntry("three", []byte("three"))})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{}})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &base})
		_ = protocol.WriteMessage(connection, protocol.Message{
			Version: protocol.Version,
			Type:    protocol.TypeDelta,
			Delta: &protocol.Delta{
				Revision: 3,
				Changes:  []protocol.Change{{Kind: protocol.ChangeAdded, ID: final.Entries[1].ID, Entry: &final.Entries[1]}},
			},
		})
		resync, err := protocol.ReadMessage(connection)
		if err != nil || resync.Type != protocol.TypeResync {
			return
		}
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, RequestID: resync.RequestID, Snapshot: &final})
	})

	client := newTestClient(t, path)
	cancel, done := startClient(t, client)
	defer cancel()
	_ = nextUpdate(t, client.Updates())
	update := nextUpdate(t, client.Updates())
	if !update.Connected || update.Message.Type != protocol.TypeSnapshot || update.Snapshot.Revision != 3 || len(update.Snapshot.Entries) != 2 {
		t.Fatalf("resync update = %+v", update)
	}
	cancel()
	<-done
}

func TestClientPublishesUnavailableAndBoundsReconnectBackoff(t *testing.T) {
	var mu sync.Mutex
	var delays []time.Duration
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{
		dial: func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("daemon unavailable")
		},
		wait: func(_ context.Context, delay time.Duration) error {
			mu.Lock()
			delays = append(delays, delay)
			stop := len(delays) >= 6
			mu.Unlock()
			if stop {
				return context.Canceled
			}
			return nil
		},
	})
	updateCh := client.Updates()
	err := client.Run(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	update := nextUpdate(t, updateCh)
	if update.Connected || update.Snapshot.Wayland != protocol.WaylandUnavailable || update.Snapshot.Persistence != protocol.PersistenceUnavailable {
		t.Fatalf("unavailable update = %+v", update)
	}
	mu.Lock()
	got := append([]time.Duration(nil), delays...)
	mu.Unlock()
	if len(got) < 6 {
		t.Fatalf("reconnect attempts = %d, want at least 6", len(got))
	}
	for index, delay := range got {
		if delay < minReconnectDelay || delay > maxReconnectDelay {
			t.Fatalf("backoff[%d] = %s, outside [%s, %s]", index, delay, minReconnectDelay, maxReconnectDelay)
		}
		if index > 0 && delay < got[index-1] {
			t.Fatalf("backoff decreased from %s to %s", got[index-1], delay)
		}
	}
}

func TestClientReportsUnavailableAfterMalformedServerMessage(t *testing.T) {
	path := startClientServer(t, func(connection net.Conn) {
		if !readHello(connection) {
			return
		}
		snapshot := testSnapshot(0, nil)
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{}})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot})
		_ = protocol.WriteFrame(connection, []byte(`{"version":1,"type":"snapshot","unexpected":true}`))
	})

	client := newTestClient(t, path)
	cancel, done := startClient(t, client)
	defer cancel()
	connected := nextUpdate(t, client.Updates())
	if !connected.Connected {
		t.Fatalf("connected update = %+v", connected)
	}
	unavailable := nextUpdate(t, client.Updates())
	if unavailable.Connected || unavailable.Snapshot.Wayland != protocol.WaylandUnavailable {
		t.Fatalf("malformed-server update = %+v", unavailable)
	}
	cancel()
	<-done
}

func TestClientHandshakeStopsWhenContextIsCancelled(t *testing.T) {
	clientConnection, serverConnection := net.Pipe()
	stopServer := make(chan struct{})
	serverReadHello := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverConnection.Close()
		if _, err := protocol.ReadFrame(serverConnection); err != nil {
			return
		}
		close(serverReadHello)
		<-stopServer
	}()
	t.Cleanup(func() {
		close(stopServer)
		_ = clientConnection.Close()
		_ = serverConnection.Close()
		<-serverDone
	})

	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{
		dial: func(context.Context, string) (net.Conn, error) {
			return clientConnection, nil
		},
		wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case <-serverReadHello:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not send handshake")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run remained blocked after handshake cancellation")
	}
}

func TestClientCommandQueueIsBoundedAndNonBlocking(t *testing.T) {
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{})
	for index := 0; index < maxCommandQueue; index++ {
		if err := client.Restore("entry"); err != nil {
			t.Fatalf("Restore(%d) error = %v", index, err)
		}
	}
	if err := client.Restore("entry"); !errors.Is(err, ErrCommandQueueFull) {
		t.Fatalf("overflow error = %v, want %v", err, ErrCommandQueueFull)
	}
	if err := client.Pin("entry", true); !errors.Is(err, ErrCommandQueueFull) {
		t.Fatalf("Pin overflow error = %v, want %v", err, ErrCommandQueueFull)
	}
	if err := client.Copy("text"); !errors.Is(err, ErrCommandQueueFull) {
		t.Fatalf("Copy overflow error = %v, want %v", err, ErrCommandQueueFull)
	}
}

func TestClientCommandsEncodeAllDaemonOperations(t *testing.T) {
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{})
	if err := client.Copy("/usr/bin/firefox"); err != nil {
		t.Fatal(err)
	}
	if err := client.Restore("entry"); err != nil {
		t.Fatal(err)
	}
	if err := client.Pin("entry", true); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete("entry"); err != nil {
		t.Fatal(err)
	}
	if err := client.Clear(protocol.ClearUnpinned); err != nil {
		t.Fatal(err)
	}
	if err := client.Thumbnail("entry", 64); err != nil {
		t.Fatal(err)
	}
	if err := client.Resync(); err != nil {
		t.Fatal(err)
	}
	want := []protocol.MessageType{
		protocol.TypeCopy,
		protocol.TypeRestore,
		protocol.TypePin,
		protocol.TypeDelete,
		protocol.TypeClear,
		protocol.TypeThumbnail,
		protocol.TypeResync,
	}
	for index, messageType := range want {
		message := <-client.commands
		if message.Type != messageType || message.RequestID == "" {
			t.Fatalf("command[%d] = %+v, want %q with request ID", index, message, messageType)
		}
		if err := protocol.ValidateMessage(message); err != nil {
			t.Fatalf("command[%d] invalid: %v", index, err)
		}
		if message.Type == protocol.TypeCopy && message.Text != "/usr/bin/firefox" {
			t.Fatalf("copy command text = %q", message.Text)
		}
	}
}

func TestClientCopyRejectsInvalidTextBeforeQueueing(t *testing.T) {
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{})
	if err := client.Copy(""); err == nil {
		t.Fatal("Copy accepted empty text")
	}
	if len(client.commands) != 0 {
		t.Fatalf("queued commands = %d, want 0", len(client.commands))
	}
}

func TestClientCopyRequiresServerCapability(t *testing.T) {
	received := make(chan protocol.Message, 1)
	path := startClientServer(t, func(connection net.Conn) {
		if !readHello(connection) {
			return
		}
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{Capabilities: []string{"restore"}}})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &protocol.Snapshot{Persistence: protocol.PersistenceUnavailable, Wayland: protocol.WaylandUnavailable}})
		message, err := protocol.ReadMessage(connection)
		if err == nil {
			received <- message
			_ = connection.Close() // An older daemon closes on an unknown message type.
		}
	})

	client := newTestClient(t, path)
	if err := client.Copy("queued before handshake"); err != nil {
		t.Fatalf("queue Copy: %v", err)
	}
	cancel, done := startClient(t, client)
	_ = nextUpdate(t, client.Updates())
	update := nextUpdate(t, client.Updates())
	if !update.Connected || update.Message.Type != protocol.TypeError || update.Message.RequestID != "request-1" || update.Message.Error == nil || update.Message.Error.Code != protocol.ErrorUnsupported || update.Message.Error.RequestID != "request-1" {
		t.Fatalf("unsupported copy update = %+v", update)
	}
	if err := client.Copy("after handshake"); !errors.Is(err, ErrCopyUnsupported) {
		t.Fatalf("Copy error = %v, want %v", err, ErrCopyUnsupported)
	}
	cancel()
	<-done
	select {
	case message := <-received:
		t.Fatalf("sent unsupported command %+v", message)
	default:
	}
}

func TestClientCopySendsWhenServerAdvertisesCapability(t *testing.T) {
	commands := make(chan protocol.Message, 1)
	path := startClientServer(t, func(connection net.Conn) {
		if !readHello(connection) {
			return
		}
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello, Hello: &protocol.Hello{Capabilities: []string{"copy"}}})
		_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &protocol.Snapshot{Persistence: protocol.PersistenceUnavailable, Wayland: protocol.WaylandUnavailable}})
		message, err := protocol.ReadMessage(connection)
		if err == nil {
			commands <- message
			_ = protocol.WriteMessage(connection, protocol.Message{Version: protocol.Version, Type: protocol.TypeAck, Ack: &protocol.Ack{RequestID: message.RequestID}})
		}
	})

	client := newTestClient(t, path)
	cancel, done := startClient(t, client)
	defer func() {
		cancel()
		<-done
	}()
	_ = nextUpdate(t, client.Updates())
	if err := client.Copy("hello"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	select {
	case message := <-commands:
		if message.Type != protocol.TypeCopy || message.Text != "hello" {
			t.Fatalf("copy command = %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("server received no copy command")
	}
	update := nextUpdate(t, client.Updates())
	if update.Message.Type != protocol.TypeAck {
		t.Fatalf("copy acknowledgement = %+v", update.Message)
	}
}

func TestRestoreWithRequestIDReturnsQueuedID(t *testing.T) {
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{})
	requestID, err := client.RestoreWithRequestID("entry")
	if err != nil {
		t.Fatalf("RestoreWithRequestID: %v", err)
	}
	message := <-client.commands
	if message.Type != protocol.TypeRestore || message.ID != "entry" || message.RequestID != requestID {
		t.Fatalf("queued restore = %+v, returned request ID = %q", message, requestID)
	}
}

func TestClientRejectsInvalidSocketPath(t *testing.T) {
	if _, err := New("relative.sock"); err == nil {
		t.Fatal("New accepted a relative socket path")
	}
	if _, err := New(""); err == nil {
		t.Fatal("New accepted an empty socket path")
	}
}

func TestApplyDeltaRejectsGapWithoutPartialState(t *testing.T) {
	entry := testEntry("one", []byte("one"))
	current := testSnapshot(1, []protocol.Entry{entry})
	second := testEntry("two", []byte("two"))
	_, err := applyDelta(current, protocol.Delta{
		Revision: 3,
		Changes:  []protocol.Change{{Kind: protocol.ChangeAdded, ID: second.ID, Entry: &second}},
	})
	if !errors.Is(err, ErrRevisionGap) {
		t.Fatalf("applyDelta error = %v, want %v", err, ErrRevisionGap)
	}
	if len(current.Entries) != 1 || current.Entries[0].ID != entry.ID {
		t.Fatalf("current snapshot changed after rejected delta: %+v", current)
	}
}

func TestClientCoalescesSlowUpdatesToLatestSnapshot(t *testing.T) {
	client := newClientWithOptions("/tmp/clipboard-test.sock", clientOptions{})
	for revision := uint64(1); revision <= inboundQueueSize; revision++ {
		snapshot := testSnapshot(revision, nil)
		client.markConnected(protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}, snapshot)
	}
	latest := testSnapshot(inboundQueueSize+1, nil)
	client.markConnected(protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &latest}, latest)

	update := nextUpdate(t, client.Updates())
	if update.Snapshot.Revision != inboundQueueSize+1 {
		t.Fatalf("coalesced snapshot revision = %d, want %d", update.Snapshot.Revision, inboundQueueSize+1)
	}
	select {
	case extra := <-client.Updates():
		t.Fatalf("coalesced queue retained stale update %+v", extra)
	default:
	}
}

func testEntry(id string, payload []byte) protocol.Entry {
	hash := sha256.Sum256(payload)
	return protocol.Entry{
		ID:          id,
		Kind:        protocol.KindText,
		MIME:        "text/plain",
		OfferedMIME: []string{"text/plain"},
		Size:        uint64(len(payload)),
		SHA256:      hex.EncodeToString(hash[:]),
		CapturedAt:  time.Unix(1, 0).UTC(),
		Preview:     string(payload),
	}
}

func testSnapshot(revision uint64, entries []protocol.Entry) protocol.Snapshot {
	return protocol.Snapshot{
		Revision:    revision,
		Entries:     entries,
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandUnavailable,
	}
}

func startClientServer(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.v1.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err == nil {
			handler(connection)
			// The connection stays open until the test finishes so that the
			// client cannot mark the session unavailable while the test is
			// still asserting on the handshake.
			<-release
			_ = connection.Close()
		}
	}()
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("test server did not stop")
		}
	})
	return path
}

func readHello(connection net.Conn) bool {
	hello, err := protocol.ReadMessage(connection)
	return err == nil && hello.Type == protocol.TypeHello
}

func newTestClient(t *testing.T, path string) *Client {
	t.Helper()
	client, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func startClient(t *testing.T, client *Client) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return cancel, done
}

func nextUpdate(t *testing.T, updates <-chan Update) Update {
	t.Helper()
	select {
	case update := <-updates:
		return update
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for clipboard client update")
		return Update{}
	}
}

package daemon

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/internal/store"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

func TestServicePublishesStartupSnapshotAndCaptureDelta(t *testing.T) {
	h := history.New(1)
	svc := NewService(Options{
		History:     h,
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandUnavailable,
	})
	t.Cleanup(svc.Close)

	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	startup := nextMessage(t, sub.Updates)
	if startup.Type != protocol.TypeSnapshot || startup.Snapshot == nil {
		t.Fatalf("startup message = %+v, want snapshot", startup)
	}
	if startup.Snapshot.Revision != 0 || len(startup.Snapshot.Entries) != 0 {
		t.Fatalf("startup snapshot = %+v", startup.Snapshot)
	}

	if err := svc.Capture(context.Background(), textCapture(1, "first", time.Unix(10, 0))); err != nil {
		t.Fatal(err)
	}
	delta := nextMessage(t, sub.Updates)
	if delta.Type != protocol.TypeDelta || delta.Delta == nil {
		t.Fatalf("capture message = %+v, want delta", delta)
	}
	if delta.Delta.Revision != 1 || len(delta.Delta.Changes) != 1 || delta.Delta.Changes[0].Kind != protocol.ChangeAdded {
		t.Fatalf("capture delta = %+v", delta.Delta)
	}
	if delta.Delta.Changes[0].Entry == nil || delta.Delta.Changes[0].Entry.Preview != "first" {
		t.Fatalf("capture change = %+v", delta.Delta.Changes[0])
	}
}

func TestServiceDuplicateCapturePublishesMoveWithoutDuplicate(t *testing.T) {
	svc := newTestService(t, history.New(1))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	if err := svc.Capture(context.Background(), textCapture(1, "same", time.Unix(10, 0))); err != nil {
		t.Fatal(err)
	}
	added := nextMessage(t, sub.Updates)
	entryID := added.Delta.Changes[0].ID
	if err := svc.Capture(context.Background(), textCapture(1, "same", time.Unix(20, 0))); err != nil {
		t.Fatal(err)
	}
	moved := nextMessage(t, sub.Updates)
	if moved.Type != protocol.TypeDelta || moved.Delta.Revision != 2 || len(moved.Delta.Changes) != 1 {
		t.Fatalf("duplicate delta = %+v", moved)
	}
	change := moved.Delta.Changes[0]
	if change.Kind != protocol.ChangeMoved || change.ID != entryID {
		t.Fatalf("duplicate change = %+v", change)
	}
	snapshot, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].ID != entryID || !snapshot.Entries[0].CapturedAt.Equal(time.Unix(20, 0).UTC()) {
		t.Fatalf("duplicate snapshot = %+v", snapshot)
	}
}

func TestServiceCommandsRestorePinDeleteAndClear(t *testing.T) {
	var restored string
	svc := NewService(Options{
		History: history.New(1),
		Restore: func(item history.Item) error {
			restored = item.Metadata().ID
			return nil
		},
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandReady,
	})
	t.Cleanup(svc.Close)
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	for _, value := range []string{"first", "second"} {
		if err := svc.Capture(context.Background(), textCapture(1, value, time.Now())); err != nil {
			t.Fatal(err)
		}
		_ = nextMessage(t, sub.Updates)
	}
	snapshot, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("captured entries = %+v", snapshot.Entries)
	}
	newestID := snapshot.Entries[0].ID
	oldestID := snapshot.Entries[1].ID

	ack := svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeRestore, RequestID: "restore-1", ID: oldestID})
	if ack.Type != protocol.TypeAck || ack.Ack == nil || ack.Ack.RequestID != "restore-1" || restored != oldestID {
		t.Fatalf("restore response = %+v, restored = %q", ack, restored)
	}
	if got := nextIfAvailable(sub.Updates); got != nil {
		t.Fatalf("restore unexpectedly published %v", *got)
	}

	pinned := true
	ack = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypePin, RequestID: "pin-1", ID: oldestID, Pinned: &pinned})
	if ack.Type != protocol.TypeAck || ack.Ack == nil || ack.Ack.RequestID != "pin-1" {
		t.Fatalf("pin response = %+v", ack)
	}
	pinDelta := nextMessage(t, sub.Updates)
	if pinDelta.Delta == nil || pinDelta.Delta.Changes[0].Kind != protocol.ChangeUpdated || pinDelta.Delta.Changes[0].Entry == nil || !pinDelta.Delta.Changes[0].Entry.Pinned {
		t.Fatalf("pin delta = %+v", pinDelta)
	}

	ack = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeDelete, RequestID: "delete-1", ID: newestID})
	if ack.Type != protocol.TypeAck || ack.Ack == nil || ack.Ack.RequestID != "delete-1" {
		t.Fatalf("delete response = %+v", ack)
	}
	deleteDelta := nextMessage(t, sub.Updates)
	if deleteDelta.Delta == nil || len(deleteDelta.Delta.Changes) != 1 || deleteDelta.Delta.Changes[0].Kind != protocol.ChangeRemoved || deleteDelta.Delta.Changes[0].ID != newestID {
		t.Fatalf("delete delta = %+v", deleteDelta)
	}

	if err := svc.Capture(context.Background(), textCapture(1, "third", time.Now())); err != nil {
		t.Fatal(err)
	}
	_ = nextMessage(t, sub.Updates)
	ack = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeClear, RequestID: "clear-1", Scope: protocol.ClearUnpinned})
	if ack.Type != protocol.TypeAck || ack.Ack == nil || ack.Ack.RequestID != "clear-1" {
		t.Fatalf("clear response = %+v", ack)
	}
	clearDelta := nextMessage(t, sub.Updates)
	if clearDelta.Delta == nil || len(clearDelta.Delta.Changes) != 1 || clearDelta.Delta.Changes[0].Kind != protocol.ChangeRemoved {
		t.Fatalf("clear delta = %+v", clearDelta)
	}
	final, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Entries) != 1 || final.Entries[0].ID != oldestID || !final.Entries[0].Pinned {
		t.Fatalf("after clear-unpinned = %+v", final.Entries)
	}

	ack = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeClear, RequestID: "clear-2", Scope: protocol.ClearAll})
	if ack.Type != protocol.TypeAck || ack.Ack == nil || ack.Ack.RequestID != "clear-2" {
		t.Fatalf("clear-all response = %+v", ack)
	}
	allDelta := nextMessage(t, sub.Updates)
	if allDelta.Delta == nil || len(allDelta.Delta.Changes) != 1 || allDelta.Delta.Changes[0].ID != oldestID {
		t.Fatalf("clear-all delta = %+v", allDelta)
	}
}

func TestServiceFailedCommandDoesNotAdvanceRevision(t *testing.T) {
	svc := newTestService(t, history.New(1))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	before, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeDelete, RequestID: "missing", ID: "does-not-exist"})
	if response.Type != protocol.TypeError || response.Error == nil || response.Error.Code != protocol.ErrorNotFound || response.Error.RequestID != "missing" {
		t.Fatalf("failed command response = %+v", response)
	}
	after, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || len(after.Entries) != len(before.Entries) {
		t.Fatalf("failed command changed snapshot from %+v to %+v", before, after)
	}
	if got := nextIfAvailable(sub.Updates); got != nil {
		t.Fatalf("failed command published %v", *got)
	}
}

func TestServicePersistenceFailurePublishesVolatileState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, err := store.New(root, bytes.Repeat([]byte{7}, store.KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Options{History: history.New(1), Store: state, Wayland: protocol.WaylandUnavailable})
	t.Cleanup(svc.Close)
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := svc.Capture(context.Background(), textCapture(1, "volatile", time.Unix(30, 0))); err != nil {
		t.Fatal(err)
	}
	message := nextMessage(t, sub.Updates)
	if message.Type != protocol.TypeDelta || message.Delta == nil || message.Delta.Persistence != protocol.PersistenceVolatile {
		t.Fatalf("volatile capture message = %+v", message)
	}
	snapshot, err := svc.CurrentSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Persistence != protocol.PersistenceVolatile || len(snapshot.Entries) != 1 {
		t.Fatalf("volatile snapshot = %+v", snapshot)
	}
}

func TestServiceSlowSubscriberCoalescesToCurrentSnapshot(t *testing.T) {
	svc := newTestService(t, history.New(1))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)

	for index := 0; index < 3; index++ {
		if err := svc.Capture(context.Background(), textCapture(1, "value-"+string(rune('a'+index)), time.Unix(int64(index+1), 0))); err != nil {
			t.Fatal(err)
		}
	}
	message := nextMessage(t, sub.Updates)
	if message.Type != protocol.TypeSnapshot || message.Snapshot == nil || message.Snapshot.Revision != 3 || len(message.Snapshot.Entries) != 3 {
		t.Fatalf("coalesced message = %+v", message)
	}
	if got := nextIfAvailable(sub.Updates); got != nil {
		t.Fatalf("slow subscriber retained more than one message: %v", *got)
	}
}

func TestServicePublishesWaylandStateChangeWithoutRevision(t *testing.T) {
	svc := newTestService(t, history.New(1))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	if err := svc.SetWaylandState(context.Background(), protocol.WaylandReady); err != nil {
		t.Fatal(err)
	}
	message := nextMessage(t, sub.Updates)
	if message.Type != protocol.TypeSnapshot || message.Snapshot == nil || message.Snapshot.Revision != 0 || message.Snapshot.Wayland != protocol.WaylandReady {
		t.Fatalf("wayland state message = %+v", message)
	}
}

func TestServiceThumbnailIsBoundedPNGAndRejectsText(t *testing.T) {
	svc := newTestService(t, history.New(1))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	var encoded bytes.Buffer
	source := image.NewRGBA(image.Rect(0, 0, 4, 2))
	source.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	if err := svc.Capture(context.Background(), history.Capture{
		Generation: 1,
		Kind:       protocol.KindImage,
		MIME:       "image/png",
		Payload:    encoded.Bytes(),
		CapturedAt: time.Unix(40, 0),
	}); err != nil {
		t.Fatal(err)
	}
	added := nextMessage(t, sub.Updates)
	id := added.Delta.Changes[0].ID
	response := svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-1", ID: id, MaxPX: 1})
	if response.Type != protocol.TypeThumbnail || response.RequestID != "thumb-1" || response.Thumbnail == nil {
		t.Fatalf("thumbnail response = %+v", response)
	}
	if err := protocol.ValidateThumbnail(*response.Thumbnail); err != nil {
		t.Fatal(err)
	}
	if response.Thumbnail.Width != 1 || response.Thumbnail.Height != 1 || len(response.Thumbnail.Data) == 0 {
		t.Fatalf("thumbnail = %+v", response.Thumbnail)
	}

	if err := svc.Capture(context.Background(), textCapture(1, "text", time.Unix(50, 0))); err != nil {
		t.Fatal(err)
	}
	textID := nextMessage(t, sub.Updates).Delta.Changes[0].ID
	textResponse := svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-2", ID: textID, MaxPX: 1})
	if textResponse.Type != protocol.TypeError || textResponse.Error == nil || textResponse.Error.Code != protocol.ErrorUnsupported {
		t.Fatalf("text thumbnail response = %+v", textResponse)
	}
}

func newTestService(t *testing.T, h *history.History) *Service {
	t.Helper()
	svc := NewService(Options{
		History:     h,
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandUnavailable,
	})
	t.Cleanup(svc.Close)
	return svc
}

func textCapture(generation uint64, value string, capturedAt time.Time) history.Capture {
	return history.Capture{
		Generation:  generation,
		Kind:        protocol.KindText,
		MIME:        "text/plain",
		OfferedMIME: []string{"text/plain", "UTF8_STRING"},
		Payload:     []byte(value),
		CapturedAt:  capturedAt,
	}
}

func nextMessage(t *testing.T, updates <-chan protocol.Message) protocol.Message {
	t.Helper()
	select {
	case message := <-updates:
		return message
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for service message")
		return protocol.Message{}
	}
}

func nextIfAvailable(updates <-chan protocol.Message) *protocol.Message {
	select {
	case message := <-updates:
		return &message
	default:
		return nil
	}
}

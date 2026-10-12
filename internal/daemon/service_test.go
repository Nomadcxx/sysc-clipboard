package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
		Restore: func(_ context.Context, item history.Item) error {
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

func TestRestoreDoesNotBlockReducerSnapshot(t *testing.T) {
	historyState := history.New(1)
	if _, err := historyState.Capture(textCapture(1, "restore target", time.Unix(10, 0))); err != nil {
		t.Fatal(err)
	}
	id := historyState.Snapshot()[0].ID
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	svc := NewService(Options{
		History:     historyState,
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandReady,
		Restore: func(context.Context, history.Item) error {
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return nil
		},
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
		svc.Close()
	}()

	restoreDone := make(chan protocol.Message, 1)
	go func() {
		restoreDone <- svc.Execute(context.Background(), protocol.Message{
			Version: protocol.Version, Type: protocol.TypeRestore, RequestID: "restore-blocked", ID: id,
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("restore callback did not start")
	}

	snapshotCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := svc.CurrentSnapshot(snapshotCtx); err != nil {
		t.Fatalf("snapshot blocked behind restore: %v", err)
	}

	queued := make([]<-chan serviceReply, 0, serviceQueueSize)
	for index := 0; index < serviceQueueSize; index++ {
		queued = append(queued, enqueueServiceCommand(t, svc, protocol.Message{
			Version: protocol.Version, Type: protocol.TypeRestore,
			RequestID: fmt.Sprintf("restore-queued-%d", index), ID: id,
		}))
	}
	busy := waitServiceReply(t, enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeRestore, RequestID: "restore-overflow", ID: id,
	}))
	if busy.message.Type != protocol.TypeError || busy.message.Error == nil || busy.message.Error.Code != protocol.ErrorUnavailable {
		t.Fatalf("overflow restore response = %+v, want unavailable", busy.message)
	}

	close(release)
	released = true
	select {
	case response := <-restoreDone:
		if response.Type != protocol.TypeAck || response.Ack == nil || response.Ack.RequestID != "restore-blocked" {
			t.Fatalf("restore response = %+v", response)
		}
	case <-time.After(time.Second):
		t.Fatal("restore did not complete")
	}
	for index, reply := range queued {
		if response := waitServiceReply(t, reply).message; response.Type != protocol.TypeAck {
			t.Fatalf("queued restore %d response = %+v", index, response)
		}
	}
}

func TestRestoreCancellationReachesWorker(t *testing.T) {
	historyState := history.New(1)
	if _, err := historyState.Capture(textCapture(1, "restore target", time.Unix(10, 0))); err != nil {
		t.Fatal(err)
	}
	id := historyState.Snapshot()[0].ID
	started := make(chan struct{})
	cancelled := make(chan struct{})
	svc := NewService(Options{
		History:     historyState,
		Persistence: protocol.PersistenceUnavailable,
		Wayland:     protocol.WaylandReady,
		Restore: func(ctx context.Context, _ history.Item) error {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		},
	})
	t.Cleanup(svc.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan protocol.Message, 1)
	go func() {
		done <- svc.Execute(ctx, protocol.Message{
			Version: protocol.Version, Type: protocol.TypeRestore, RequestID: "restore-cancelled", ID: id,
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("restore callback did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("restore callback did not receive cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled restore did not return")
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

func TestServiceSetGenerationRejectsOlderCaptureWithoutPublishing(t *testing.T) {
	svc := newTestService(t, history.New(0))
	sub, err := svc.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sub.Close)
	_ = nextMessage(t, sub.Updates)

	if err := svc.SetGeneration(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.Capture(context.Background(), textCapture(0, "stale", time.Unix(1, 0))); !errors.Is(err, history.ErrStaleGeneration) {
		t.Fatalf("stale capture error = %v, want %v", err, history.ErrStaleGeneration)
	}
	if err := svc.Capture(context.Background(), textCapture(1, "current", time.Unix(2, 0))); err != nil {
		t.Fatal(err)
	}
	message := nextMessage(t, sub.Updates)
	if message.Type != protocol.TypeDelta || message.Delta == nil || message.Delta.Revision != 1 || message.Delta.Changes[0].Entry.Preview != "current" {
		t.Fatalf("capture after generation change = %+v", message)
	}
}

func TestServiceThumbnailIsBoundedPNGAndRejectsText(t *testing.T) {
	svc := newTestService(t, history.New(1))
	thumbnailizeCalls := 0
	svc.thumbnailize = func(item history.Item, maxPX uint16) (protocol.Thumbnail, error) {
		thumbnailizeCalls++
		return makeThumbnail(item, maxPX)
	}
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
	response = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-cached", ID: id, MaxPX: 1})
	if response.Type != protocol.TypeThumbnail || response.Thumbnail == nil || thumbnailizeCalls != 1 {
		t.Fatalf("cached thumbnail response = %+v, thumbnailize calls = %d", response, thumbnailizeCalls)
	}
	response = svc.Execute(context.Background(), protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-larger", ID: id, MaxPX: 2})
	if response.Type != protocol.TypeThumbnail || response.Thumbnail == nil || response.Thumbnail.Width != 2 || thumbnailizeCalls != 2 {
		t.Fatalf("size-specific thumbnail response = %+v, thumbnailize calls = %d", response, thumbnailizeCalls)
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

func TestThumbnailWorkLeavesReducerResponsiveAndRejectsStaleResults(t *testing.T) {
	historyState := history.New(1)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	changes, err := historyState.Capture(history.Capture{
		Generation: 1, Kind: protocol.KindImage, MIME: "image/png", Payload: encoded.Bytes(), CapturedAt: time.Unix(30, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].ID
	svc := newTestService(t, historyState)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	svc.thumbnailize = func(item history.Item, _ uint16) (protocol.Thumbnail, error) {
		started <- struct{}{}
		<-release
		return protocol.Thumbnail{ID: item.Metadata().ID, MIME: "image/png", Width: 1, Height: 1, Data: []byte{1}}, nil
	}

	first := enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-held", ID: id, MaxPX: 1,
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("thumbnail worker did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := svc.CurrentSnapshot(ctx); err != nil {
		t.Fatalf("snapshot blocked behind thumbnail: %v", err)
	}

	second := enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-queued", ID: id, MaxPX: 1,
	})
	third := enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-overflow", ID: id, MaxPX: 1,
	})
	deleted := svc.Execute(context.Background(), protocol.Message{
		Version: protocol.Version, Type: protocol.TypeDelete, RequestID: "delete-during-thumbnail", ID: id,
	})
	if deleted.Type != protocol.TypeAck {
		t.Fatalf("delete during thumbnail = %+v", deleted)
	}

	close(release)
	released = true
	// A burst queues behind the single worker instead of failing, and every
	// completion is rejected because the item was deleted mid-flight.
	for label, reply := range map[string]<-chan serviceReply{"active": first, "queued": second, "overflow": third} {
		response := waitServiceReply(t, reply).message
		if response.Type != protocol.TypeError || response.Error == nil || response.Error.Code != protocol.ErrorNotFound {
			t.Fatalf("%s stale thumbnail response = %+v, want not found", label, response)
		}
	}
}

func TestCanceledThumbnailDoesNotStallQueue(t *testing.T) {
	historyState := history.New(1)
	changes, err := historyState.Capture(imageCapture(1))
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].ID
	svc := newTestService(t, historyState)
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	svc.thumbnailize = func(item history.Item, _ uint16) (protocol.Thumbnail, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return protocol.Thumbnail{ID: item.Metadata().ID, MIME: "image/png", Width: 1, Height: 1, Data: []byte{1}}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstReply := make(chan serviceReply, 1)
	if err := svc.submitRequest(ctx, serviceRequest{
		kind: requestCommand, ctx: ctx,
		message: protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-canceled", ID: id, MaxPX: 1},
		reply:   firstReply,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CurrentSnapshot(context.Background()); err != nil {
		t.Fatalf("wait for first thumbnail dispatch: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("thumbnail worker did not start")
	}

	cancel()
	close(release)
	released = true
	second := enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "thumb-after-cancel", ID: id, MaxPX: 1,
	})
	response := waitServiceReply(t, second).message
	if response.Type != protocol.TypeThumbnail || response.Thumbnail == nil || response.Thumbnail.ID != id {
		t.Fatalf("thumbnail after cancellation = %+v, want thumbnail for %q", response, id)
	}
}

// A burst larger than the worker depth must queue, not fail: a history UI asks
// for a whole screen of thumbnails at once.
func TestThumbnailBurstQueuesInsteadOfFailing(t *testing.T) {
	const burst = 8
	historyState := history.New(1)
	ids := make([]string, 0, burst)
	for index := 0; index < burst; index++ {
		changes, err := historyState.Capture(imageCapture(byte(index)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, changes[0].ID)
	}
	if len(ids) != burst {
		t.Fatalf("captured %d distinct entries, want %d", len(ids), burst)
	}
	svc := newTestService(t, historyState)

	responses := make([]protocol.Message, burst)
	var group sync.WaitGroup
	group.Add(burst)
	for index, id := range ids {
		go func() {
			defer group.Done()
			responses[index] = svc.Execute(context.Background(), protocol.Message{
				Version: protocol.Version, Type: protocol.TypeThumbnail,
				RequestID: fmt.Sprintf("burst-%d", index), ID: id, MaxPX: 1,
			})
		}()
	}
	group.Wait()
	for index, response := range responses {
		if response.Type != protocol.TypeThumbnail || response.Thumbnail == nil {
			t.Fatalf("burst thumbnail %d response = %+v, want thumbnail", index, response)
		}
		if response.Thumbnail.ID != ids[index] {
			t.Fatalf("burst thumbnail %d id = %q, want %q", index, response.Thumbnail.ID, ids[index])
		}
	}
}

// Queueing must not become unbounded: past the cap the reducer still sheds.
func TestThumbnailPendingQueueIsCapped(t *testing.T) {
	historyState := history.New(1)
	changes, err := historyState.Capture(imageCapture(1))
	if err != nil {
		t.Fatal(err)
	}
	id := changes[0].ID
	svc := newTestService(t, historyState)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	svc.thumbnailize = func(item history.Item, _ uint16) (protocol.Thumbnail, error) {
		<-release
		return makeThumbnail(item, 1)
	}

	// The first request occupies the worker, so request n leaves n-1 queued.
	for index := 1; index <= maxPendingThumbnails+1; index++ {
		enqueueServiceCommand(t, svc, protocol.Message{
			Version: protocol.Version, Type: protocol.TypeThumbnail,
			RequestID: fmt.Sprintf("cap-%d", index), ID: id, MaxPX: 1,
		})
	}
	over := waitServiceReply(t, enqueueServiceCommand(t, svc, protocol.Message{
		Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: "cap-over", ID: id, MaxPX: 1,
	}))
	if over.message.Type != protocol.TypeError || over.message.Error == nil || over.message.Error.Code != protocol.ErrorUnavailable {
		t.Fatalf("capped thumbnail response = %+v, want unavailable", over.message)
	}
	close(release)
	released = true
}

// A restore that expired while queued never reached the Wayland owner, so it
// must not report the context error as if the clipboard had refused it.
func TestRestoreThatExpiredInQueueReportsNotStarted(t *testing.T) {
	state := &reducerState{pendingRestores: map[uint64]pendingRestore{}}
	reply := make(chan serviceReply, 1)
	state.pendingRestores[1] = pendingRestore{itemID: "id", cancel: func() {}}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	state.completeRestore(restoreResult{
		id: 1, ctx: expired, requestID: "restore-expired",
		started: false, err: context.DeadlineExceeded, reply: reply,
	})
	message := (<-reply).message
	if message.Type != protocol.TypeError || message.Error == nil || !strings.Contains(message.Error.Message, ErrRestoreNotStarted.Error()) {
		t.Fatalf("expired restore response = %+v, want %v", message, ErrRestoreNotStarted)
	}
	if len(state.pendingRestores) != 0 {
		t.Fatalf("pendingRestores = %d, want drained", len(state.pendingRestores))
	}
}

// imageCapture encodes a 1x1 PNG whose only pixel varies, so each capture is a
// distinct entry.
func imageCapture(seed byte) history.Capture {
	decoded := image.NewGray(image.Rect(0, 0, 1, 1))
	decoded.SetGray(0, 0, color.Gray{Y: seed})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, decoded); err != nil {
		panic(err)
	}
	return history.Capture{
		Generation: 1, Kind: protocol.KindImage, MIME: "image/png",
		Payload: encoded.Bytes(), CapturedAt: time.Unix(int64(seed), 0),
	}
}

func TestThumbnailCacheBoundsEntriesAndBytes(t *testing.T) {
	cache := newThumbnailCache()
	for index := 0; index < maxThumbnailCacheEntries+1; index++ {
		id := fmt.Sprintf("entry-%d", index)
		identity := protocol.Entry{ID: id, Kind: protocol.KindImage, MIME: "image/png", Size: 1, SHA256: id}
		cache.put(thumbnailKey{id: id, maxPX: 1}, identity, protocol.Thumbnail{Data: []byte{1}})
	}
	if len(cache.entries) != maxThumbnailCacheEntries {
		t.Fatalf("cache entries = %d, want %d", len(cache.entries), maxThumbnailCacheEntries)
	}
	if _, ok := cache.get(thumbnailKey{id: "entry-0", maxPX: 1}, protocol.Entry{ID: "entry-0", Kind: protocol.KindImage, MIME: "image/png", Size: 1, SHA256: "entry-0"}); ok {
		t.Fatal("cache retained its oldest entry after reaching the entry limit")
	}

	cache = newThumbnailCache()
	for index := 0; index < 3; index++ {
		id := fmt.Sprintf("large-%d", index)
		identity := protocol.Entry{ID: id, Kind: protocol.KindImage, MIME: "image/png", Size: 1, SHA256: id}
		cache.put(thumbnailKey{id: id, maxPX: 1}, identity, protocol.Thumbnail{Data: make([]byte, maxThumbnailCacheBytes/2)})
	}
	if cache.bytes > maxThumbnailCacheBytes || len(cache.entries) != 2 {
		t.Fatalf("byte-bounded cache has %d entries and %d bytes", len(cache.entries), cache.bytes)
	}
	oldIdentity := protocol.Entry{ID: "large-1", Kind: protocol.KindImage, MIME: "image/png", Size: 1, SHA256: "large-1"}
	changedIdentity := oldIdentity
	changedIdentity.SHA256 = "changed"
	if _, ok := cache.get(thumbnailKey{id: "large-1", maxPX: 1}, changedIdentity); ok {
		t.Fatal("cache returned a thumbnail for a changed entry identity")
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

func enqueueServiceCommand(t *testing.T, svc *Service, message protocol.Message) <-chan serviceReply {
	t.Helper()
	ctx := context.Background()
	reply := make(chan serviceReply, 1)
	if err := svc.submitRequest(ctx, serviceRequest{kind: requestCommand, ctx: ctx, message: message, reply: reply}); err != nil {
		t.Fatal(err)
	}
	barrierCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := svc.CurrentSnapshot(barrierCtx); err != nil {
		t.Fatalf("wait for command dispatch: %v", err)
	}
	return reply
}

func waitServiceReply(t *testing.T, reply <-chan serviceReply) serviceReply {
	t.Helper()
	select {
	case result := <-reply:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for service reply")
		return serviceReply{}
	}
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

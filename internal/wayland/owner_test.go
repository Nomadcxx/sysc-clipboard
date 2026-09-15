package wayland

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
	wlclient "github.com/Nomadcxx/sysc-wayland/client"
	"golang.org/x/sys/unix"
)

func TestChooseGlobalsPrefersExtAndFirstSeat(t *testing.T) {
	globals := []globalAnnouncement{
		{name: 7, interfaceName: protocolWLRManagerInterface, version: 2},
		{name: 4, interfaceName: seatInterface, version: 9},
		{name: 2, interfaceName: protocolExtManagerInterface, version: 1},
		{name: 5, interfaceName: seatInterface, version: 7},
	}

	manager, seat, ok := chooseGlobals(globals)
	if !ok {
		t.Fatal("chooseGlobals reported no usable globals")
	}
	if manager.interfaceName != protocolExtManagerInterface || manager.name != 2 {
		t.Fatalf("manager = %+v, want ext manager", manager)
	}
	if seat.name != 4 || seat.version != 9 {
		t.Fatalf("seat = %+v, want first seat", seat)
	}
}

func TestChooseGlobalsFallsBackToWLR(t *testing.T) {
	manager, seat, ok := chooseGlobals([]globalAnnouncement{
		{name: 3, interfaceName: seatInterface, version: 1},
		{name: 9, interfaceName: protocolWLRManagerInterface, version: 4},
	})
	if !ok || manager.interfaceName != protocolWLRManagerInterface || manager.name != 9 || seat.name != 3 {
		t.Fatalf("chooseGlobals = %+v, %+v, %v", manager, seat, ok)
	}
}

func TestManagerVersionIsClampedToImplementedProtocol(t *testing.T) {
	tests := []struct {
		name          string
		interfaceName string
		server        uint32
		want          uint32
	}{
		{name: "ext server newer", interfaceName: protocolExtManagerInterface, server: 9, want: 1},
		{name: "wlr server newer", interfaceName: protocolWLRManagerInterface, server: 9, want: 2},
		{name: "wlr server older", interfaceName: protocolWLRManagerInterface, server: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := managerVersion(tt.interfaceName, tt.server); got != tt.want {
				t.Fatalf("managerVersion(%q, %d) = %d, want %d", tt.interfaceName, tt.server, got, tt.want)
			}
		})
	}
}

func TestSelectCaptureMIMEPrefersTextAndThenImages(t *testing.T) {
	tests := []struct {
		name     string
		mimes    []string
		wantKind protocol.Kind
		wantMIME string
		wantOK   bool
	}{
		{name: "preferred text", mimes: []string{"text/plain", "text/plain;charset=utf-8"}, wantKind: protocol.KindText, wantMIME: "text/plain;charset=utf-8", wantOK: true},
		{name: "UTF8 alias", mimes: []string{"UTF8_STRING"}, wantKind: protocol.KindText, wantMIME: "UTF8_STRING", wantOK: true},
		{name: "preferred image", mimes: []string{"image/jpeg", "image/png"}, wantKind: protocol.KindImage, wantMIME: "image/png", wantOK: true},
		{name: "unsupported", mimes: []string{"text/html", "application/octet-stream"}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, mime, ok := selectCaptureMIME(tt.mimes)
			if kind != tt.wantKind || mime != tt.wantMIME || ok != tt.wantOK {
				t.Fatalf("selectCaptureMIME(%v) = %q, %q, %v", tt.mimes, kind, mime, ok)
			}
		})
	}
}

func TestRestoreMIMEsAddsOnlySafeTextAliases(t *testing.T) {
	got := restoreMIMEs(protocol.Entry{Kind: protocol.KindText, MIME: "text/plain;charset=utf-8"})
	want := []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING"}
	if !equalStrings(got, want) {
		t.Fatalf("restoreMIMEs = %v, want %v", got, want)
	}

	got = restoreMIMEs(protocol.Entry{Kind: protocol.KindImage, MIME: "image/png"})
	if !equalStrings(got, []string{"image/png"}) {
		t.Fatalf("image restore MIME list = %v", got)
	}
}

func TestOwnerIgnoresPrimaryAndCapturesTextAndImageMIMEs(t *testing.T) {
	backend := newFakeBackend()
	captures := make(chan history.Capture, 2)
	owner := runTestOwner(t, backend, OwnerOptions{
		Capture: func(_ context.Context, capture history.Capture) error {
			captures <- capture
			return nil
		},
	})
	_ = owner

	primary := &fakeOffer{idValue: 1, payload: []byte("secret primary")}
	text := &fakeOffer{idValue: 2, payload: []byte("regular text")}
	backend.push(func() {
		backend.device.emitDataOffer(primary)
		primary.emitMIME("text/plain")
		backend.device.emitPrimary(primary)
		backend.device.emitDataOffer(text)
		text.emitMIME("text/plain")
		backend.device.emitSelection(text)
	})

	first := nextCapture(t, captures)
	if first.Kind != protocol.KindText || first.MIME != "text/plain" || string(first.Payload) != "regular text" || first.Generation != 1 {
		t.Fatalf("text capture = %+v", first)
	}
	if !equalStrings(first.OfferedMIME, []string{"text/plain"}) {
		t.Fatalf("text offered MIME = %v", first.OfferedMIME)
	}
	if got := len(captures); got != 0 {
		t.Fatalf("primary selection produced %d captures", got)
	}

	imageOffer := &fakeOffer{idValue: 3, payload: []byte("image bytes")}
	backend.push(func() {
		backend.device.emitDataOffer(imageOffer)
		imageOffer.emitMIME("image/jpeg")
		imageOffer.emitMIME("image/png")
		backend.device.emitSelection(imageOffer)
	})
	second := nextCapture(t, captures)
	if second.Kind != protocol.KindImage || second.MIME != "image/png" || string(second.Payload) != "image bytes" || second.Generation != 2 {
		t.Fatalf("image capture = %+v", second)
	}
}

func TestOwnerDoesNotBlockWaylandDispatchOnCapturePersistence(t *testing.T) {
	backend := newFakeBackend()
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	var captureOnce sync.Once
	runTestOwner(t, backend, OwnerOptions{
		Capture: func(ctx context.Context, _ history.Capture) error {
			captureOnce.Do(func() { close(captureStarted) })
			select {
			case <-releaseCapture:
			case <-ctx.Done():
			}
			return nil
		},
	})

	first := &fakeOffer{idValue: 30, payload: []byte("first")}
	backend.push(func() {
		backend.device.emitDataOffer(first)
		first.emitMIME("text/plain")
		backend.device.emitSelection(first)
	})
	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("capture callback did not start")
	}

	second := &fakeOffer{idValue: 31, payload: []byte("second"), receiveStarted: make(chan struct{})}
	backend.push(func() {
		backend.device.emitDataOffer(second)
		second.emitMIME("text/plain")
		backend.device.emitSelection(second)
	})
	select {
	case <-second.receiveStarted:
	case <-time.After(250 * time.Millisecond):
		close(releaseCapture)
		t.Fatal("Wayland dispatch stopped behind capture persistence")
	}
	close(releaseCapture)
}

func TestOwnerSerializesCapturePersistence(t *testing.T) {
	backend := newFakeBackend()
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })
	var valuesMu sync.Mutex
	var values []string
	runTestOwner(t, backend, OwnerOptions{
		Capture: func(_ context.Context, capture history.Capture) error {
			value := string(capture.Payload)
			valuesMu.Lock()
			values = append(values, value)
			valuesMu.Unlock()
			switch value {
			case "first":
				close(firstStarted)
				<-releaseFirst
			case "second":
				close(secondStarted)
			}
			return nil
		},
	})

	first := &fakeOffer{idValue: 32, payload: []byte("first")}
	backend.push(func() {
		backend.device.emitDataOffer(first)
		first.emitMIME("text/plain")
		backend.device.emitSelection(first)
	})
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first capture callback did not start")
	}

	second := &fakeOffer{idValue: 33, payload: []byte("second"), receiveStarted: make(chan struct{})}
	backend.push(func() {
		backend.device.emitDataOffer(second)
		second.emitMIME("text/plain")
		backend.device.emitSelection(second)
	})
	select {
	case <-second.receiveStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Wayland dispatch stopped behind capture persistence")
	}
	select {
	case <-secondStarted:
		t.Fatal("capture callbacks ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseFirst) })
	select {
	case <-secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second capture callback did not complete")
	}

	valuesMu.Lock()
	defer valuesMu.Unlock()
	if len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("capture callback order = %v, want [first second]", values)
	}
}

func TestOwnerUsesKindLimitBeforeCapture(t *testing.T) {
	backend := newFakeBackend()
	captures := make(chan history.Capture, 1)
	owner := runTestOwner(t, backend, OwnerOptions{
		Capture: func(_ context.Context, capture history.Capture) error {
			captures <- capture
			return nil
		},
	})
	_ = owner

	offer := &fakeOffer{idValue: 34, payload: bytes.Repeat([]byte("x"), protocol.MaxTextBytes+1)}
	backend.push(func() {
		backend.device.emitDataOffer(offer)
		offer.emitMIME("text/plain")
		backend.device.emitSelection(offer)
	})
	waitFor(t, offer.isDestroyed)
	select {
	case capture := <-captures:
		t.Fatalf("oversized text was captured: %d bytes", len(capture.Payload))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOwnerClosesReplacedAndCancelledPendingReadFDs(t *testing.T) {
	firstReader, firstWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer firstWriter.Close()
	defer firstReader.Close()
	secondReader, secondWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer secondWriter.Close()
	defer secondReader.Close()

	owner := newOwner(newFakeBackend(), OwnerOptions{})
	owner.readBusy = true
	firstFD := int(firstReader.Fd())
	secondFD := int(secondReader.Fd())
	owner.pendingRead = &pendingReadRequest{fd: firstFD}
	owner.queueRead(readRequest{}, secondFD)
	if _, err := unix.FcntlInt(uintptr(firstFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("replaced pending read FD error = %v, want EBADF", err)
	}

	owner.cancelRead()
	if _, err := unix.FcntlInt(uintptr(secondFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("cancelled pending read FD error = %v, want EBADF", err)
	}
}

func TestOwnerDoesNotBlockWaylandDispatchOnGenerationPublication(t *testing.T) {
	backend := newFakeBackend()
	generationStarted := make(chan struct{})
	releaseGeneration := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseGeneration) }) })
	runTestOwner(t, backend, OwnerOptions{
		SetGeneration: func(ctx context.Context, _ uint64) error {
			close(generationStarted)
			select {
			case <-releaseGeneration:
			case <-ctx.Done():
			}
			return nil
		},
	})

	offer := &fakeOffer{idValue: 35, payload: []byte("generation"), receiveStarted: make(chan struct{})}
	backend.push(func() {
		backend.device.emitDataOffer(offer)
		offer.emitMIME("text/plain")
		backend.device.emitSelection(offer)
	})
	select {
	case <-generationStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("generation callback did not start")
	}
	select {
	case <-offer.receiveStarted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Wayland dispatch stopped behind generation publication")
	}
	releaseOnce.Do(func() { close(releaseGeneration) })
}

func TestOwnerReportsCaptureQueueSaturation(t *testing.T) {
	reports := make(chan error, 1)
	captureStarted := make(chan struct{})
	releaseCapture := make(chan struct{})
	var captureOnce sync.Once
	var releaseOnce sync.Once
	owner := newOwner(newFakeBackend(), OwnerOptions{
		Capture: func(_ context.Context, _ history.Capture) error {
			captureOnce.Do(func() { close(captureStarted) })
			<-releaseCapture
			return nil
		},
		CaptureError: func(err error) { reports <- err },
	})
	ctx, cancel := context.WithCancel(context.Background())
	go owner.captureLoop(ctx)
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseCapture) })
		cancel()
		close(owner.captureJobs)
		<-owner.captureDone
		close(owner.captureErrors)
	})
	owner.captureJobs <- captureJob{}
	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("capture callback did not start")
	}
	for index := 0; index < maxCaptureQueue; index++ {
		owner.captureJobs <- captureJob{}
	}
	if owner.enqueueCapture(captureJob{}) {
		t.Fatal("capture queue accepted an item past its bound")
	}
	releaseOnce.Do(func() { close(releaseCapture) })
	select {
	case err := <-reports:
		if !errors.Is(err, ErrCaptureQueueFull) {
			t.Fatalf("capture overflow error = %v, want %v", err, ErrCaptureQueueFull)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture queue saturation was not reported")
	}
}

func TestOwnerCancelsCapturePersistenceOnShutdown(t *testing.T) {
	backend := newFakeBackend()
	captureStarted := make(chan struct{})
	owner := newOwner(backend, OwnerOptions{
		Capture: func(ctx context.Context, capture history.Capture) error {
			close(captureStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- owner.Run(ctx) }()
	select {
	case <-owner.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("owner did not become ready")
	}
	offer := &fakeOffer{idValue: 36, payload: []byte("shutdown")}
	backend.push(func() {
		backend.device.emitDataOffer(offer)
		offer.emitMIME("text/plain")
		backend.device.emitSelection(offer)
	})
	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("capture callback did not start")
	}
	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not stop after capture cancellation")
	}
}

func TestOwnerDiscardsStaleReadResult(t *testing.T) {
	var captures []history.Capture
	owner := newOwner(newFakeBackend(), OwnerOptions{
		Capture: func(_ context.Context, capture history.Capture) error {
			captures = append(captures, capture)
			return nil
		},
	})
	owner.generation = 2
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner.readBusy = true
	owner.currentRead = &readState{request: readRequest{generation: 1, offerID: 4}, cancel: cancel}
	owner.handleReadResult(readResult{
		readRequest: readRequest{
			generation: 1,
			offerID:    4,
			kind:       protocol.KindText,
			mime:       "text/plain",
		},
		payload: []byte("old selection"),
	})
	if len(captures) != 0 || owner.backup != nil {
		t.Fatalf("stale read changed owner: captures=%d backup=%v", len(captures), owner.backup)
	}
}

func TestOwnerRestoreKeepsSourceUntilCancellation(t *testing.T) {
	historyState := history.New(1)
	if _, err := historyState.Capture(history.Capture{
		Generation:  1,
		Kind:        protocol.KindText,
		MIME:        "text/plain;charset=utf-8",
		OfferedMIME: []string{"text/plain;charset=utf-8"},
		Payload:     []byte("restore me"),
		CapturedAt:  time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	item := historyState.Items()[0]
	backend := newFakeBackend()
	owner := runTestOwner(t, backend, OwnerOptions{})

	if err := owner.Restore(item); err != nil {
		t.Fatal(err)
	}
	if got := backend.manager.sourceCount(); got != 1 {
		t.Fatalf("source count = %d, want one", got)
	}
	source := backend.manager.source(1)
	if source.isDestroyed() {
		t.Fatal("restore source destroyed before cancellation")
	}
	if !equalStrings(source.mimes, []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING"}) {
		t.Fatalf("restore source MIME list = %v", source.mimes)
	}

	backend.push(func() { source.emitCancelled() })
	waitFor(t, source.isDestroyed)
}

func TestOwnerRestoresOnlyAfterDeletedOfferIsAcknowledged(t *testing.T) {
	backend := newFakeBackend()
	owner := runTestOwner(t, backend, OwnerOptions{})
	offer := &fakeOffer{idValue: 20}
	backend.push(func() {
		owner.offers[offer.idValue] = &offerState{offer: offer}
		owner.offerOrder = append(owner.offerOrder, offer.idValue)
		owner.currentOffer = offer
		owner.currentOfferID = offer.idValue
		owner.destroyOffer(offer.idValue)
	})
	waitFor(t, offer.isDestroyed)
	before := backend.roundtripCount

	itemHistory := history.New(1)
	if _, err := itemHistory.Capture(history.Capture{
		Generation: 1,
		Kind:       protocol.KindText,
		MIME:       "text/plain",
		Payload:    []byte("restore after delete"),
		CapturedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Restore(itemHistory.Items()[0]); err != nil {
		t.Fatal(err)
	}
	if backend.roundtripCount <= before {
		t.Fatalf("restore roundtrips = %d, want more than %d", backend.roundtripCount, before)
	}
}

func TestOwnerRetriesInterruptedWaylandWait(t *testing.T) {
	backend := newFakeBackend()
	backend.waitError = unix.EINTR
	owner := newOwner(backend, OwnerOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- owner.Run(ctx) }()

	select {
	case <-owner.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not become ready")
	}
	backend.push(owner.handleFinished)

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrOwnerFinished) {
			t.Fatalf("owner error = %v, want %v after interrupted wait", err, ErrOwnerFinished)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner stopped waiting after EINTR")
	}
}

func TestOwnerReadAndWriteWorkersAreBoundedAndComplete(t *testing.T) {
	backend := newFakeBackend()
	owner := runTestOwner(t, backend, OwnerOptions{})
	itemHistory := history.New(1)
	if _, err := itemHistory.Capture(history.Capture{
		Generation:  1,
		Kind:        protocol.KindText,
		MIME:        "text/plain",
		OfferedMIME: []string{"text/plain"},
		Payload:     []byte("worker payload"),
		CapturedAt:  time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Restore(itemHistory.Items()[0]); err != nil {
		t.Fatal(err)
	}
	source := backend.manager.source(1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	backend.push(func() { source.emitSend("text/plain", int(writer.Fd())) })
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != "worker payload" {
		t.Fatalf("written payload = %q", data)
	}
}

func TestOwnerReadResultReAdoptionDoesNotDuplicateHistory(t *testing.T) {
	historyState := history.New(1)
	var historyMu sync.Mutex
	backend := newFakeBackend()
	owner := runTestOwner(t, backend, OwnerOptions{
		SetGeneration: func(_ context.Context, generation uint64) error {
			historyMu.Lock()
			defer historyMu.Unlock()
			historyState.SetGeneration(generation)
			return nil
		},
		Capture: func(_ context.Context, capture history.Capture) error {
			historyMu.Lock()
			defer historyMu.Unlock()
			_, err := historyState.Capture(capture)
			return err
		},
	})

	firstOffer := &fakeOffer{idValue: 10, payload: []byte("same clipboard")}
	backend.push(func() {
		backend.device.emitDataOffer(firstOffer)
		firstOffer.emitMIME("text/plain")
		backend.device.emitSelection(firstOffer)
	})
	waitFor(t, func() bool {
		historyMu.Lock()
		defer historyMu.Unlock()
		return historyState.Count() == 1
	})

	backend.push(func() { backend.device.emitSelection(nil) })
	waitFor(t, backend.device.hasSelection)

	adopted := &fakeOffer{idValue: 11, payload: []byte("same clipboard")}
	backend.push(func() {
		backend.device.emitDataOffer(adopted)
		adopted.emitMIME("text/plain")
		backend.device.emitSelection(adopted)
	})
	waitFor(t, func() bool {
		historyMu.Lock()
		defer historyMu.Unlock()
		return historyState.Count() == 1 && adopted.isDestroyed()
	})
	historyMu.Lock()
	defer historyMu.Unlock()
	if got := historyState.Count(); got != 1 {
		t.Fatalf("re-adoption created %d history entries", got)
	}
	_ = owner
}

func TestOwnerCleanupDestroysCurrentOfferOnce(t *testing.T) {
	owner := newOwner(newFakeBackend(), OwnerOptions{})
	offer := &fakeOffer{idValue: 20}
	owner.offers[offer.idValue] = &offerState{offer: offer}
	owner.offerOrder = []uint32{offer.idValue}
	owner.currentOffer = offer
	owner.currentOfferID = offer.idValue

	owner.cleanupProxies()
	if got := offer.destroyCountValue(); got != 1 {
		t.Fatalf("current offer destroy count = %d, want one", got)
	}
}

func TestOwnerForgetsDestroyedOfferID(t *testing.T) {
	owner := newOwner(newFakeBackend(), OwnerOptions{})
	offer := &fakeOffer{idValue: 21}
	owner.offers[offer.idValue] = &offerState{offer: offer}
	owner.offerOrder = []uint32{offer.idValue}

	owner.destroyOffer(offer.idValue)
	if got := offer.releaseCountValue(); got != 1 {
		t.Fatalf("offer release count = %d, want one", got)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

type fakeBackend struct {
	manager        *fakeManager
	device         *fakeDevice
	events         chan func()
	next           func()
	roundtripCount int
	waitError      error
}

func newFakeBackend() *fakeBackend {
	manager := &fakeManager{nextID: 1, sources: make(map[uint32]*fakeSource)}
	return &fakeBackend{manager: manager, device: &fakeDevice{}, events: make(chan func(), 32)}
}

func (backend *fakeBackend) bind() (dataControlManager, dataControlDevice, error) {
	return backend.manager, backend.device, nil
}

func (backend *fakeBackend) roundtrip() error {
	backend.roundtripCount++
	return nil
}

func (backend *fakeBackend) wait(timeout time.Duration) (bool, error) {
	if backend.waitError != nil {
		err := backend.waitError
		backend.waitError = nil
		return false, err
	}
	select {
	case backend.next = <-backend.events:
		return true, nil
	case <-time.After(timeout):
		return false, nil
	}
}

func (backend *fakeBackend) dispatch() error {
	if backend.next == nil {
		return errors.New("fake backend dispatched without an event")
	}
	event := backend.next
	backend.next = nil
	event()
	return nil
}

func (backend *fakeBackend) close() error { return nil }

func (backend *fakeBackend) push(event func()) { backend.events <- event }

type fakeManager struct {
	mu      sync.Mutex
	nextID  uint32
	sources map[uint32]*fakeSource
}

func (manager *fakeManager) createSource() (dataControlSource, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	source := &fakeSource{idValue: manager.nextID}
	manager.nextID++
	manager.sources[source.idValue] = source
	return source, nil
}

func (manager *fakeManager) source(id uint32) *fakeSource {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.sources[id]
}

func (manager *fakeManager) sourceCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return len(manager.sources)
}

func (manager *fakeManager) getDevice(*wlclient.Seat) (dataControlDevice, error) {
	return nil, errors.New("fake manager does not bind devices")
}

func (manager *fakeManager) destroy() error { return nil }

type fakeDevice struct {
	mu               sync.Mutex
	dataOfferHandler func(dataControlOffer)
	selectionHandler func(dataControlOffer)
	primaryHandler   func(dataControlOffer)
	finishedHandler  func()
	selection        dataControlSource
}

func (device *fakeDevice) setDataOfferHandler(handler func(dataControlOffer)) {
	device.dataOfferHandler = handler
}

func (device *fakeDevice) setSelectionHandler(handler func(dataControlOffer)) {
	device.selectionHandler = handler
}

func (device *fakeDevice) setPrimarySelectionHandler(handler func(dataControlOffer)) {
	device.primaryHandler = handler
}

func (device *fakeDevice) setFinishedHandler(handler func()) { device.finishedHandler = handler }

func (device *fakeDevice) setSelection(source dataControlSource) error {
	device.mu.Lock()
	defer device.mu.Unlock()
	device.selection = source
	return nil
}

func (device *fakeDevice) hasSelection() bool {
	device.mu.Lock()
	defer device.mu.Unlock()
	return device.selection != nil
}

func (device *fakeDevice) destroy() error { return nil }

func (device *fakeDevice) emitDataOffer(offer dataControlOffer) {
	if device.dataOfferHandler != nil {
		device.dataOfferHandler(offer)
	}
}

func (device *fakeDevice) emitSelection(offer dataControlOffer) {
	if device.selectionHandler != nil {
		device.selectionHandler(offer)
	}
}

func (device *fakeDevice) emitPrimary(offer dataControlOffer) {
	if device.primaryHandler != nil {
		device.primaryHandler(offer)
	}
}

type fakeOffer struct {
	mu             sync.Mutex
	idValue        uint32
	payload        []byte
	mimeHandler    func(string)
	receiveMIMEs   []string
	destroyed      bool
	destroyCount   int
	releaseCount   int
	receiveStarted chan struct{}
	receiveOnce    sync.Once
}

func (offer *fakeOffer) id() uint32 { return offer.idValue }

func (offer *fakeOffer) setOfferHandler(handler func(string)) { offer.mimeHandler = handler }

func (offer *fakeOffer) receive(mime string, fd int) error {
	offer.receiveMIMEs = append(offer.receiveMIMEs, mime)
	offer.receiveOnce.Do(func() {
		if offer.receiveStarted != nil {
			close(offer.receiveStarted)
		}
	})
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	payload := append([]byte(nil), offer.payload...)
	go func() {
		if len(payload) > 0 {
			_, _ = unix.Write(dup, payload)
		}
		_ = unix.Close(dup)
	}()
	return nil
}

func (offer *fakeOffer) destroy() error {
	offer.mu.Lock()
	defer offer.mu.Unlock()
	offer.destroyed = true
	offer.destroyCount++
	return nil
}

func (offer *fakeOffer) release() {
	offer.mu.Lock()
	defer offer.mu.Unlock()
	offer.releaseCount++
}

func (offer *fakeOffer) isDestroyed() bool {
	offer.mu.Lock()
	defer offer.mu.Unlock()
	return offer.destroyed
}

func (offer *fakeOffer) destroyCountValue() int {
	offer.mu.Lock()
	defer offer.mu.Unlock()
	return offer.destroyCount
}

func (offer *fakeOffer) releaseCountValue() int {
	offer.mu.Lock()
	defer offer.mu.Unlock()
	return offer.releaseCount
}

func (offer *fakeOffer) emitMIME(mime string) {
	if offer.mimeHandler != nil {
		offer.mimeHandler(mime)
	}
}

type fakeSource struct {
	mu            sync.Mutex
	idValue       uint32
	mimes         []string
	payload       []byte
	sendHandler   func(sourceSendEvent)
	cancelHandler func()
	destroyed     bool
}

func (source *fakeSource) id() uint32 { return source.idValue }

func (source *fakeSource) offer(mime string) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.mimes = append(source.mimes, mime)
	return nil
}

func (source *fakeSource) setSendHandler(handler func(sourceSendEvent)) { source.sendHandler = handler }

func (source *fakeSource) setCancelledHandler(handler func()) { source.cancelHandler = handler }

func (source *fakeSource) destroy() error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.destroyed = true
	return nil
}

func (source *fakeSource) isDestroyed() bool {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.destroyed
}

func (source *fakeSource) emitCancelled() {
	if source.cancelHandler != nil {
		source.cancelHandler()
	}
}

func (source *fakeSource) emitSend(mime string, fd int) {
	if source.sendHandler != nil {
		source.sendHandler(sourceSendEvent{mime: mime, fd: fd})
	}
}

func runTestOwner(t *testing.T, backend *fakeBackend, options OwnerOptions) *Owner {
	t.Helper()
	owner := newOwner(backend, options)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- owner.Run(ctx) }()
	select {
	case <-owner.ready:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("owner did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("owner did not stop")
		}
	})
	return owner
}

func nextCapture(t *testing.T, captures <-chan history.Capture) history.Capture {
	t.Helper()
	select {
	case capture := <-captures:
		return capture
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for capture")
		return history.Capture{}
	}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

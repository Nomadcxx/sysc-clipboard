package wayland

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
	wlclient "github.com/Nomadcxx/sysc-wayland/client"
	"golang.org/x/sys/unix"
)

const (
	protocolExtManagerInterface = "ext_data_control_manager_v1"
	protocolWLRManagerInterface = "zwlr_data_control_manager_v1"
	seatInterface               = "wl_seat"

	maxSelectionBackupBytes = 200 << 20
	maxOfferStates          = 64
	maxCaptureQueue         = 4
	maxWriteQueue           = 4
	maxOutgoingSources      = 4
	ownerCommandQueue       = 4
	ownerPollInterval       = 25 * time.Millisecond
	seatBindingVersion      = 10
)

var (
	ErrOwnerUnavailable = errors.New("clipboard Wayland owner is unavailable")
	ErrOwnerRunning     = errors.New("clipboard Wayland owner is already running")
	ErrOwnerFinished    = errors.New("clipboard data-control device finished")
	ErrCaptureQueueFull = errors.New("clipboard capture queue is full")
	ErrSourceLimit      = errors.New("clipboard source queue is full")
)

type globalAnnouncement struct {
	name          uint32
	interfaceName string
	version       uint32
}

type OwnerOptions struct {
	// Capture runs serially off the Wayland owner goroutine. The context is
	// cancelled when the owner stops.
	Capture func(context.Context, history.Capture) error
	// CaptureError receives capture queue and persistence failures off the
	// Wayland owner goroutine.
	CaptureError func(error)
	// SetGeneration runs in the same ordered worker as Capture.
	SetGeneration   func(context.Context, uint64) error
	SetWaylandState func(protocol.WaylandState) error
}

// The generated ext and wlr bindings have parallel but distinct Go types. The
// normalized interfaces keep that protocol detail at the edge and make the
// state machine fakeable without touching generated proxies.
type dataControlManager interface {
	createSource() (dataControlSource, error)
	getDevice(*wlclient.Seat) (dataControlDevice, error)
	destroy() error
}

type dataControlDevice interface {
	setDataOfferHandler(func(dataControlOffer))
	setSelectionHandler(func(dataControlOffer))
	setPrimarySelectionHandler(func(dataControlOffer))
	setFinishedHandler(func())
	setSelection(dataControlSource) error
	destroy() error
}

type sourceSendEvent struct {
	mime string
	fd   int
}

type dataControlSource interface {
	id() uint32
	offer(string) error
	setSendHandler(func(sourceSendEvent))
	setCancelledHandler(func())
	destroy() error
}

type dataControlOffer interface {
	id() uint32
	setOfferHandler(func(string))
	receive(string, int) error
	destroy() error
	release()
}

type waylandBackend interface {
	bind() (dataControlManager, dataControlDevice, error)
	roundtrip() error
	wait(time.Duration) (bool, error)
	dispatch() error
	close() error
}

type Owner struct {
	backend waylandBackend
	options OwnerOptions

	manager dataControlManager
	device  dataControlDevice

	commands chan ownerCommand
	ready    chan struct{}
	done     chan struct{}
	started  chan struct{}
	runOnce  sync.Once

	readJobs      chan readJob
	readResults   chan readResult
	readDone      chan struct{}
	captureJobs   chan captureJob
	captureErrors chan error
	captureDone   chan struct{}
	writeJobs     chan writeJob
	writeDone     chan struct{}

	generation    uint64
	generationErr error
	finished      error
	stateReady    bool

	offers         map[uint32]*offerState
	offerOrder     []uint32
	currentOffer   dataControlOffer
	currentOfferID uint32

	readBusy    bool
	currentRead *readState
	pendingRead *pendingReadRequest
	backup      *selectionBackup
	// A server-created offer stays registered in the Wayland context until
	// wl_display.delete_id arrives. Do not create a replacement selection
	// before that acknowledgement, because a compositor may reuse its ID.
	offerDeletionPending bool

	sources        map[uint32]*sourceState
	awaitingSource uint32
}

type ownerCommand struct {
	item  history.Item
	reply chan error
}

type offerState struct {
	offer   dataControlOffer
	mimes   []string
	invalid bool
}

type readRequest struct {
	generation uint64
	offerID    uint32
	kind       protocol.Kind
	mime       string
	offered    []string
}

type pendingReadRequest struct {
	request readRequest
	fd      int
}

type readState struct {
	request readRequest
	cancel  context.CancelFunc
}

type readJob struct {
	request readRequest
	ctx     context.Context
	fd      int
}

type readResult struct {
	readRequest
	payload []byte
	err     error
}

type selectionBackup struct {
	kind    protocol.Kind
	mime    string
	payload []byte
}

type sourceState struct {
	source  dataControlSource
	mimes   []string
	payload []byte
	ctx     context.Context
	cancel  context.CancelFunc
}

type writeJob struct {
	ctx     context.Context
	fd      int
	payload []byte
}

type captureJob struct {
	generation    uint64
	setGeneration bool
	capture       history.Capture
}

func newOwner(backend waylandBackend, options OwnerOptions) *Owner {
	return &Owner{
		backend:       backend,
		options:       options,
		commands:      make(chan ownerCommand, ownerCommandQueue),
		ready:         make(chan struct{}),
		done:          make(chan struct{}),
		started:       make(chan struct{}),
		readJobs:      make(chan readJob, 1),
		readResults:   make(chan readResult, 1),
		readDone:      make(chan struct{}),
		captureJobs:   make(chan captureJob, maxCaptureQueue),
		captureErrors: make(chan error, 1),
		captureDone:   make(chan struct{}),
		writeJobs:     make(chan writeJob, maxWriteQueue),
		writeDone:     make(chan struct{}),
		offers:        make(map[uint32]*offerState),
		sources:       make(map[uint32]*sourceState),
	}
}

// NewOwner opens the compositor connection. A connection failure is returned
// to the caller so the daemon can continue serving loaded history as
// Wayland-unavailable.
func NewOwner(options OwnerOptions) (*Owner, error) {
	backend, err := newRealBackend()
	if err != nil {
		return nil, err
	}
	return newOwner(backend, options), nil
}

func (owner *Owner) Run(ctx context.Context) error {
	first := false
	owner.runOnce.Do(func() {
		first = true
		close(owner.started)
	})
	if !first {
		return ErrOwnerRunning
	}
	defer close(owner.done)

	go owner.readLoop()
	captureContext, cancelCapture := context.WithCancel(ctx)
	go owner.captureLoop(captureContext)
	go owner.writeLoop()
	defer func() {
		cancelCapture()
		owner.cancelRead()
		owner.cancelSources()
		close(owner.readJobs)
		close(owner.captureJobs)
		close(owner.writeJobs)
		<-owner.readDone
		<-owner.captureDone
		<-owner.writeDone
		close(owner.captureErrors)
		owner.cleanupProxies()
		owner.publishWayland(protocol.WaylandUnavailable)
		if owner.backend != nil {
			_ = owner.backend.close()
		}
	}()

	if owner.backend == nil {
		return ErrOwnerUnavailable
	}
	manager, device, err := owner.backend.bind()
	if err != nil {
		return err
	}
	owner.manager = manager
	owner.device = device
	owner.configureDevice()
	if err := owner.backend.roundtrip(); err != nil {
		return err
	}
	owner.stateReady = true
	owner.publishWayland(protocol.WaylandReady)
	close(owner.ready)

	for {
		if owner.finished != nil {
			return owner.finished
		}
		if err := owner.processCommandOrResult(ctx); err != nil {
			return err
		}
		ready, err := owner.backend.wait(ownerPollInterval)
		if err != nil {
			return err
		}
		if !ready {
			continue
		}
		if err := owner.backend.dispatch(); err != nil {
			return err
		}
	}
}

func (owner *Owner) processCommandOrResult(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case command := <-owner.commands:
		command.reply <- owner.restore(command.item)
	case result := <-owner.readResults:
		owner.handleReadResult(result)
	default:
		return nil
	}
	if owner.finished != nil {
		return owner.finished
	}
	return nil
}

func (owner *Owner) configureDevice() {
	owner.device.setDataOfferHandler(owner.handleDataOffer)
	owner.device.setSelectionHandler(owner.handleSelection)
	owner.device.setPrimarySelectionHandler(owner.handlePrimarySelection)
	owner.device.setFinishedHandler(owner.handleFinished)
}

func (owner *Owner) publishWayland(state protocol.WaylandState) {
	if owner.options.SetWaylandState != nil {
		_ = owner.options.SetWaylandState(state)
	}
}

func (owner *Owner) Restore(item history.Item) error {
	if owner.backend == nil {
		return ErrOwnerUnavailable
	}
	valid, err := history.NewItem(item.Metadata(), item.Payload())
	if err != nil {
		return err
	}
	select {
	case <-owner.started:
	case <-owner.done:
		return ErrOwnerUnavailable
	}
	select {
	case <-owner.ready:
	case <-owner.done:
		return ErrOwnerUnavailable
	}

	reply := make(chan error, 1)
	request := ownerCommand{item: valid, reply: reply}
	select {
	case owner.commands <- request:
	case <-owner.done:
		return ErrOwnerUnavailable
	}
	select {
	case err := <-reply:
		return err
	case <-owner.done:
		return ErrOwnerUnavailable
	}
}

func (owner *Owner) handleDataOffer(offer dataControlOffer) {
	if offer == nil {
		return
	}
	id := offer.id()
	if id == 0 {
		destroyOfferProxy(offer)
		return
	}
	if _, exists := owner.offers[id]; exists {
		return
	}
	for len(owner.offerOrder) >= maxOfferStates {
		removed := false
		for _, oldID := range owner.offerOrder {
			if oldID != owner.currentOfferID {
				owner.destroyOffer(oldID)
				removed = true
				break
			}
		}
		if !removed {
			destroyOfferProxy(offer)
			return
		}
	}
	state := &offerState{offer: offer}
	owner.offers[id] = state
	owner.offerOrder = append(owner.offerOrder, id)
	offer.setOfferHandler(func(mime string) { owner.addOfferMIME(id, mime) })
}

func (owner *Owner) addOfferMIME(id uint32, mime string) {
	state := owner.offers[id]
	if state == nil || state.invalid {
		return
	}
	if !validOfferMIME(mime) || containsString(state.mimes, mime) {
		if !validOfferMIME(mime) {
			state.invalid = true
		}
		return
	}
	if len(state.mimes) >= protocol.MaxOfferedMIME {
		state.invalid = true
		return
	}
	state.mimes = append(state.mimes, mime)
}

func (owner *Owner) handlePrimarySelection(offer dataControlOffer) {
	if offer == nil || offer.id() == owner.currentOfferID {
		return
	}
	if _, exists := owner.offers[offer.id()]; exists {
		owner.destroyOffer(offer.id())
		return
	}
	destroyOfferProxy(offer)
}

func (owner *Owner) handleSelection(offer dataControlOffer) {
	owner.generation++
	owner.generationErr = nil
	if owner.options.SetGeneration != nil && !owner.enqueueCapture(captureJob{
		generation:    owner.generation,
		setGeneration: true,
	}) {
		owner.generationErr = ErrCaptureQueueFull
	}

	owner.cancelRead()
	if owner.currentOfferID != 0 && (offer == nil || offer.id() != owner.currentOfferID) {
		owner.destroyOffer(owner.currentOfferID)
	}
	owner.currentOffer = offer
	if offer == nil {
		owner.currentOfferID = 0
		if owner.awaitingSource == 0 {
			owner.adoptBackup()
		}
		return
	}
	owner.currentOfferID = offer.id()
	owner.awaitingSource = 0
	state := owner.offers[owner.currentOfferID]
	if state == nil || state.invalid || owner.generationErr != nil {
		return
	}
	kind, mime, ok := selectCaptureMIME(state.mimes)
	if !ok {
		return
	}
	reader, writer, err := newTransferPipe()
	if err != nil {
		return
	}
	if err := offer.receive(mime, writer); err != nil {
		closeFD(reader)
		closeFD(writer)
		return
	}
	closeFD(writer)
	owner.queueRead(readRequest{
		generation: owner.generation,
		offerID:    offer.id(),
		kind:       kind,
		mime:       mime,
		offered:    append([]string(nil), state.mimes...),
	}, reader)
}

func (owner *Owner) queueRead(request readRequest, fd int) {
	if owner.readBusy {
		if owner.currentRead != nil {
			owner.currentRead.cancel()
		}
		if owner.pendingRead != nil {
			closeFD(owner.pendingRead.fd)
		}
		owner.pendingRead = &pendingReadRequest{request: request, fd: fd}
		return
	}
	readContext, cancel := context.WithCancel(context.Background())
	owner.currentRead = &readState{request: request, cancel: cancel}
	owner.readBusy = true
	select {
	case owner.readJobs <- readJob{request: request, ctx: readContext, fd: fd}:
	default:
		cancel()
		closeFD(fd)
		owner.currentRead = nil
		owner.readBusy = false
	}
}

func (owner *Owner) readLoop() {
	defer close(owner.readDone)
	for job := range owner.readJobs {
		limit := protocol.MaxImageBytes
		if job.request.kind == protocol.KindText {
			limit = protocol.MaxTextBytes
		}
		payload, err := readFD(job.ctx, job.fd, limit)
		owner.readResults <- readResult{readRequest: job.request, payload: payload, err: err}
	}
}

func (owner *Owner) handleReadResult(result readResult) {
	if !owner.readBusy {
		return
	}
	current := owner.currentRead
	owner.currentRead = nil
	owner.readBusy = false
	if current != nil && result.generation == owner.generation && result.offerID == current.request.offerID && result.err == nil {
		owner.acceptPayload(result)
	}
	if result.offerID == owner.currentOfferID {
		owner.destroyOffer(result.offerID)
		owner.currentOffer = nil
		owner.currentOfferID = 0
	}
	if owner.pendingRead != nil {
		pending := *owner.pendingRead
		owner.pendingRead = nil
		owner.queueRead(pending.request, pending.fd)
	}
}

func (owner *Owner) acceptPayload(result readResult) {
	if len(result.payload) == 0 {
		return
	}
	backupPayload := bytes.Clone(result.payload)
	owner.backup = &selectionBackup{
		kind:    result.kind,
		mime:    result.mime,
		payload: backupPayload,
	}
	if owner.options.Capture != nil {
		capture := history.Capture{
			Generation:  result.generation,
			Kind:        result.kind,
			MIME:        result.mime,
			OfferedMIME: append([]string(nil), result.offered...),
			Payload:     bytes.Clone(result.payload),
			CapturedAt:  time.Now().UTC(),
		}
		owner.enqueueCapture(captureJob{generation: result.generation, capture: capture})
	}
}

func (owner *Owner) handleFinished() { owner.finished = ErrOwnerFinished }

func (owner *Owner) restore(item history.Item) error {
	if owner.manager == nil || owner.device == nil || owner.finished != nil {
		return ErrOwnerUnavailable
	}
	metadata := item.Metadata()
	payload := item.Payload()
	return owner.offerPayload(metadata.Kind, metadata.MIME, payload)
}

func (owner *Owner) offerPayload(kind protocol.Kind, mime string, payload []byte) error {
	if len(owner.sources) >= maxOutgoingSources {
		return ErrSourceLimit
	}
	mimes := restoreMIMEs(protocol.Entry{Kind: kind, MIME: mime})
	if len(mimes) == 0 || len(payload) == 0 {
		return ErrOwnerUnavailable
	}
	if owner.offerDeletionPending {
		owner.offerDeletionPending = false
		if err := owner.backend.roundtrip(); err != nil {
			owner.offerDeletionPending = true
			return err
		}
	}
	source, err := owner.manager.createSource()
	if err != nil {
		return err
	}
	if _, exists := owner.sources[source.id()]; exists {
		_ = source.destroy()
		return ErrSourceLimit
	}
	writeContext, cancel := context.WithCancel(context.Background())
	state := &sourceState{source: source, mimes: mimes, payload: payload, ctx: writeContext, cancel: cancel}
	owner.sources[source.id()] = state
	source.setSendHandler(func(event sourceSendEvent) { owner.handleSourceSend(source.id(), event) })
	source.setCancelledHandler(func() { owner.handleSourceCancelled(source.id()) })
	for _, offered := range mimes {
		if err := source.offer(offered); err != nil {
			owner.removeSource(source.id())
			return err
		}
	}
	if err := owner.device.setSelection(source); err != nil {
		owner.removeSource(source.id())
		return err
	}
	owner.awaitingSource = source.id()
	if len(payload) <= maxSelectionBackupBytes {
		owner.backup = &selectionBackup{kind: kind, mime: mime, payload: payload}
	}
	return nil
}

func (owner *Owner) handleSourceSend(id uint32, event sourceSendEvent) {
	state := owner.sources[id]
	if state == nil || event.fd < 0 || !containsString(state.mimes, event.mime) {
		closeFD(event.fd)
		return
	}
	select {
	case owner.writeJobs <- writeJob{ctx: state.ctx, fd: event.fd, payload: state.payload}:
	default:
		closeFD(event.fd)
	}
}

func (owner *Owner) handleSourceCancelled(id uint32) {
	if _, exists := owner.sources[id]; !exists {
		return
	}
	owner.removeSource(id)
	if owner.awaitingSource == id {
		owner.awaitingSource = 0
		if owner.currentOffer == nil {
			owner.adoptBackup()
		}
	}
}

func (owner *Owner) removeSource(id uint32) {
	state := owner.sources[id]
	if state == nil {
		return
	}
	state.cancel()
	delete(owner.sources, id)
	_ = state.source.destroy()
}

func (owner *Owner) adoptBackup() {
	if owner.backup == nil || owner.manager == nil || owner.device == nil || owner.awaitingSource != 0 {
		return
	}
	backup := owner.backup
	_ = owner.offerPayload(backup.kind, backup.mime, backup.payload)
}

func (owner *Owner) cancelRead() {
	if owner.currentRead != nil {
		owner.currentRead.cancel()
	}
	if owner.pendingRead != nil {
		closeFD(owner.pendingRead.fd)
	}
	owner.pendingRead = nil
}

func (owner *Owner) cancelSources() {
	for id := range owner.sources {
		owner.removeSource(id)
	}
	owner.awaitingSource = 0
}

func (owner *Owner) destroyOffer(id uint32) {
	state := owner.offers[id]
	if state == nil {
		return
	}
	destroyOfferProxy(state.offer)
	owner.offerDeletionPending = true
	delete(owner.offers, id)
	for index, oldID := range owner.offerOrder {
		if oldID == id {
			owner.offerOrder = append(owner.offerOrder[:index], owner.offerOrder[index+1:]...)
			break
		}
	}
}

func destroyOfferProxy(offer dataControlOffer) {
	if offer == nil {
		return
	}
	_ = offer.destroy()
	offer.release()
}

func (owner *Owner) cleanupProxies() {
	if owner.currentOffer != nil {
		current := owner.currentOffer
		if owner.currentOfferID != 0 && owner.offers[owner.currentOfferID] != nil {
			owner.destroyOffer(owner.currentOfferID)
		} else {
			destroyOfferProxy(current)
		}
		owner.currentOffer = nil
		owner.currentOfferID = 0
	}
	for _, id := range append([]uint32(nil), owner.offerOrder...) {
		owner.destroyOffer(id)
	}
	if owner.device != nil {
		_ = owner.device.destroy()
	}
	if owner.manager != nil {
		_ = owner.manager.destroy()
	}
}

func (owner *Owner) writeLoop() {
	defer close(owner.writeDone)
	for job := range owner.writeJobs {
		_ = writeFD(job.ctx, job.fd, job.payload)
	}
}

func (owner *Owner) enqueueCapture(job captureJob) bool {
	select {
	case owner.captureJobs <- job:
		return true
	default:
		owner.reportCaptureError(ErrCaptureQueueFull)
		return false
	}
}

func (owner *Owner) reportCaptureError(err error) {
	if owner.options.CaptureError == nil || err == nil {
		return
	}
	select {
	case owner.captureErrors <- err:
	default:
	}
}

func (owner *Owner) captureLoop(ctx context.Context) {
	defer close(owner.captureDone)
	jobs := owner.captureJobs
	errorReports := owner.captureErrors
	workerGeneration := uint64(0)
	var generationErr error
	for jobs != nil || errorReports != nil {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errorReports:
			if !ok {
				errorReports = nil
				continue
			}
			if owner.options.CaptureError != nil {
				owner.options.CaptureError(err)
			}
		case job, ok := <-jobs:
			if !ok {
				jobs = nil
				continue
			}
			if job.setGeneration {
				workerGeneration = job.generation
				generationErr = nil
				if owner.options.SetGeneration != nil {
					generationErr = owner.options.SetGeneration(ctx, job.generation)
					if generationErr != nil {
						owner.reportCaptureError(generationErr)
					}
				}
				continue
			}
			if owner.options.SetGeneration != nil && (job.generation != workerGeneration || generationErr != nil) {
				continue
			}
			if owner.options.Capture != nil {
				if err := owner.options.Capture(ctx, job.capture); err != nil {
					owner.reportCaptureError(err)
				}
			}
		}
	}
}

func chooseGlobals(globals []globalAnnouncement) (manager, seat globalAnnouncement, ok bool) {
	for _, global := range globals {
		if global.interfaceName == seatInterface && seat.name == 0 && global.version > 0 {
			seat = global
		}
	}
	for _, global := range globals {
		if global.interfaceName == protocolExtManagerInterface && global.version > 0 {
			manager = global
			break
		}
	}
	if manager.name == 0 {
		for _, global := range globals {
			if global.interfaceName == protocolWLRManagerInterface && global.version > 0 {
				manager = global
				break
			}
		}
	}
	return manager, seat, manager.name != 0 && seat.name != 0
}

func managerVersion(interfaceName string, serverVersion uint32) uint32 {
	switch interfaceName {
	case protocolExtManagerInterface:
		return min(serverVersion, uint32(1))
	case protocolWLRManagerInterface:
		return min(serverVersion, uint32(2))
	default:
		return 0
	}
}

var captureTextMIMEs = []string{
	"text/plain;charset=utf-8",
	"text/plain",
	"UTF8_STRING",
}

var captureImageMIMEs = []string{
	"image/png",
	"image/jpeg",
	"image/gif",
}

func selectCaptureMIME(offered []string) (kind protocol.Kind, mime string, ok bool) {
	for _, preferred := range captureTextMIMEs {
		if containsString(offered, preferred) {
			return protocol.KindText, preferred, true
		}
	}
	for _, preferred := range captureImageMIMEs {
		if containsString(offered, preferred) {
			return protocol.KindImage, preferred, true
		}
	}
	return "", "", false
}

func restoreMIMEs(entry protocol.Entry) []string {
	if entry.MIME == "" {
		return nil
	}
	mimes := []string{entry.MIME}
	if entry.Kind == protocol.KindText && containsString(captureTextMIMEs, entry.MIME) {
		for _, mime := range captureTextMIMEs {
			if !containsString(mimes, mime) {
				mimes = append(mimes, mime)
			}
		}
	}
	return mimes
}

func validOfferMIME(mime string) bool {
	if len(mime) == 0 || len([]byte(mime)) > protocol.MaxMIMEBytes || !utf8.ValidString(mime) || strings.TrimSpace(mime) != mime {
		return false
	}
	for _, r := range mime {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type realBackend struct {
	display wlclient.WaylandDisplay
	context *wlclient.Context
}

func newRealBackend() (*realBackend, error) {
	display, err := wlclient.Connect("")
	if err != nil {
		return nil, err
	}
	return &realBackend{display: display, context: display.Context()}, nil
}

func (backend *realBackend) bind() (dataControlManager, dataControlDevice, error) {
	registry, err := backend.display.GetRegistry()
	if err != nil {
		return nil, nil, err
	}
	globals := make([]globalAnnouncement, 0, 8)
	registry.SetGlobalHandler(func(event wlclient.RegistryGlobalEvent) {
		globals = append(globals, globalAnnouncement{name: event.Name, interfaceName: event.Interface, version: event.Version})
	})
	if err := backend.display.Roundtrip(); err != nil {
		return nil, nil, err
	}
	managerGlobal, seatGlobal, ok := chooseGlobals(globals)
	if !ok {
		return nil, nil, ErrOwnerUnavailable
	}

	seat := wlclient.NewSeat(backend.context)
	seatVersion := min(seatGlobal.version, uint32(seatBindingVersion))
	if err := registry.Bind(seatGlobal.name, wlclient.SeatInterfaceName, seatVersion, seat); err != nil {
		return nil, nil, err
	}
	version := managerVersion(managerGlobal.interfaceName, managerGlobal.version)
	if version == 0 {
		return nil, nil, ErrOwnerUnavailable
	}
	switch managerGlobal.interfaceName {
	case protocolExtManagerInterface:
		proxy := NewExtDataControlManagerV1(backend.context)
		if err := registry.Bind(managerGlobal.name, ExtDataControlManagerV1InterfaceName, version, proxy); err != nil {
			return nil, nil, err
		}
		manager := &extManager{proxy: proxy}
		device, err := manager.getDevice(seat)
		if err != nil {
			_ = manager.destroy()
			return nil, nil, err
		}
		return manager, device, nil
	case protocolWLRManagerInterface:
		proxy := NewZwlrDataControlManagerV1(backend.context)
		if err := registry.Bind(managerGlobal.name, ZwlrDataControlManagerV1InterfaceName, version, proxy); err != nil {
			return nil, nil, err
		}
		manager := &wlrManager{proxy: proxy}
		device, err := manager.getDevice(seat)
		if err != nil {
			_ = manager.destroy()
			return nil, nil, err
		}
		return manager, device, nil
	default:
		return nil, nil, ErrOwnerUnavailable
	}
}

func (backend *realBackend) roundtrip() error { return backend.display.Roundtrip() }

func (backend *realBackend) wait(timeout time.Duration) (ready bool, err error) {
	err = backend.context.ControlFD(func(fd int) error {
		milliseconds := int(timeout / time.Millisecond)
		if milliseconds < 1 {
			milliseconds = 1
		}
		pollFD := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, pollErr := unix.Poll(pollFD, milliseconds)
		if pollErr != nil {
			return pollErr
		}
		ready = n > 0 && pollFD[0].Revents&(unix.POLLIN|unix.POLLERR|unix.POLLHUP) != 0
		return nil
	})
	return ready, err
}

func (backend *realBackend) dispatch() error { return backend.context.Dispatch() }

func (backend *realBackend) close() error { return backend.context.Close() }

type extManager struct{ proxy *ExtDataControlManagerV1 }

func (manager *extManager) createSource() (dataControlSource, error) {
	source, err := manager.proxy.CreateDataSource()
	if err != nil {
		return nil, err
	}
	return &extSource{proxy: source}, nil
}

func (manager *extManager) getDevice(seat *wlclient.Seat) (dataControlDevice, error) {
	device, err := manager.proxy.GetDataDevice(seat)
	if err != nil {
		return nil, err
	}
	return &extDevice{proxy: device}, nil
}

func (manager *extManager) destroy() error { return manager.proxy.Destroy() }

type extDevice struct{ proxy *ExtDataControlDeviceV1 }

func (device *extDevice) setDataOfferHandler(handler func(dataControlOffer)) {
	device.proxy.SetDataOfferHandler(func(event ExtDataControlDeviceV1DataOfferEvent) {
		if event.Id != nil {
			handler(&extOffer{proxy: event.Id})
		}
	})
}

func (device *extDevice) setSelectionHandler(handler func(dataControlOffer)) {
	device.proxy.SetSelectionHandler(func(event ExtDataControlDeviceV1SelectionEvent) {
		if event.Id == nil {
			handler(nil)
			return
		}
		handler(&extOffer{proxy: event.Id})
	})
}

func (device *extDevice) setPrimarySelectionHandler(handler func(dataControlOffer)) {
	device.proxy.SetPrimarySelectionHandler(func(event ExtDataControlDeviceV1PrimarySelectionEvent) {
		if event.Id == nil {
			handler(nil)
			return
		}
		handler(&extOffer{proxy: event.Id})
	})
}

func (device *extDevice) setFinishedHandler(handler func()) {
	device.proxy.SetFinishedHandler(func(ExtDataControlDeviceV1FinishedEvent) { handler() })
}

func (device *extDevice) setSelection(source dataControlSource) error {
	value, ok := source.(*extSource)
	if !ok {
		return fmt.Errorf("clipboard source belongs to another data-control protocol")
	}
	return device.proxy.SetSelection(value.proxy)
}

func (device *extDevice) destroy() error { return device.proxy.Destroy() }

type extSource struct{ proxy *ExtDataControlSourceV1 }

func (source *extSource) id() uint32              { return source.proxy.ID() }
func (source *extSource) offer(mime string) error { return source.proxy.Offer(mime) }
func (source *extSource) destroy() error          { return source.proxy.Destroy() }
func (source *extSource) setSendHandler(handler func(sourceSendEvent)) {
	source.proxy.SetSendHandler(func(event ExtDataControlSourceV1SendEvent) {
		handler(sourceSendEvent{mime: event.MimeType, fd: event.Fd})
	})
}
func (source *extSource) setCancelledHandler(handler func()) {
	source.proxy.SetCancelledHandler(func(ExtDataControlSourceV1CancelledEvent) { handler() })
}

type extOffer struct{ proxy *ExtDataControlOfferV1 }

func (offer *extOffer) id() uint32                        { return offer.proxy.ID() }
func (offer *extOffer) receive(mime string, fd int) error { return offer.proxy.Receive(mime, fd) }
func (offer *extOffer) destroy() error                    { return offer.proxy.Destroy() }
func (offer *extOffer) release() {
	// Niri reuses data-control offer IDs before emitting delete_id. The
	// destroy request is already queued, and destroyed offers receive no
	// valid events, so remove the proxy at this boundary.
	offer.proxy.Context().DeleteID(offer.proxy.ID())
}
func (offer *extOffer) setOfferHandler(handler func(string)) {
	offer.proxy.SetOfferHandler(func(event ExtDataControlOfferV1OfferEvent) { handler(event.MimeType) })
}

type wlrManager struct{ proxy *ZwlrDataControlManagerV1 }

func (manager *wlrManager) createSource() (dataControlSource, error) {
	source, err := manager.proxy.CreateDataSource()
	if err != nil {
		return nil, err
	}
	return &wlrSource{proxy: source}, nil
}

func (manager *wlrManager) getDevice(seat *wlclient.Seat) (dataControlDevice, error) {
	device, err := manager.proxy.GetDataDevice(seat)
	if err != nil {
		return nil, err
	}
	return &wlrDevice{proxy: device}, nil
}

func (manager *wlrManager) destroy() error { return manager.proxy.Destroy() }

type wlrDevice struct{ proxy *ZwlrDataControlDeviceV1 }

func (device *wlrDevice) setDataOfferHandler(handler func(dataControlOffer)) {
	device.proxy.SetDataOfferHandler(func(event ZwlrDataControlDeviceV1DataOfferEvent) {
		if event.Id != nil {
			handler(&wlrOffer{proxy: event.Id})
		}
	})
}

func (device *wlrDevice) setSelectionHandler(handler func(dataControlOffer)) {
	device.proxy.SetSelectionHandler(func(event ZwlrDataControlDeviceV1SelectionEvent) {
		if event.Id == nil {
			handler(nil)
			return
		}
		handler(&wlrOffer{proxy: event.Id})
	})
}

func (device *wlrDevice) setPrimarySelectionHandler(handler func(dataControlOffer)) {
	device.proxy.SetPrimarySelectionHandler(func(event ZwlrDataControlDeviceV1PrimarySelectionEvent) {
		if event.Id == nil {
			handler(nil)
			return
		}
		handler(&wlrOffer{proxy: event.Id})
	})
}

func (device *wlrDevice) setFinishedHandler(handler func()) {
	device.proxy.SetFinishedHandler(func(ZwlrDataControlDeviceV1FinishedEvent) { handler() })
}

func (device *wlrDevice) setSelection(source dataControlSource) error {
	value, ok := source.(*wlrSource)
	if !ok {
		return fmt.Errorf("clipboard source belongs to another data-control protocol")
	}
	return device.proxy.SetSelection(value.proxy)
}

func (device *wlrDevice) destroy() error { return device.proxy.Destroy() }

type wlrSource struct{ proxy *ZwlrDataControlSourceV1 }

func (source *wlrSource) id() uint32              { return source.proxy.ID() }
func (source *wlrSource) offer(mime string) error { return source.proxy.Offer(mime) }
func (source *wlrSource) destroy() error          { return source.proxy.Destroy() }
func (source *wlrSource) setSendHandler(handler func(sourceSendEvent)) {
	source.proxy.SetSendHandler(func(event ZwlrDataControlSourceV1SendEvent) {
		handler(sourceSendEvent{mime: event.MimeType, fd: event.Fd})
	})
}
func (source *wlrSource) setCancelledHandler(handler func()) {
	source.proxy.SetCancelledHandler(func(ZwlrDataControlSourceV1CancelledEvent) { handler() })
}

type wlrOffer struct{ proxy *ZwlrDataControlOfferV1 }

func (offer *wlrOffer) id() uint32                        { return offer.proxy.ID() }
func (offer *wlrOffer) receive(mime string, fd int) error { return offer.proxy.Receive(mime, fd) }
func (offer *wlrOffer) destroy() error                    { return offer.proxy.Destroy() }
func (offer *wlrOffer) release() {
	// Keep the wlroots adapter consistent with the ext adapter above.
	offer.proxy.Context().DeleteID(offer.proxy.ID())
}
func (offer *wlrOffer) setOfferHandler(handler func(string)) {
	offer.proxy.SetOfferHandler(func(event ZwlrDataControlOfferV1OfferEvent) { handler(event.MimeType) })
}

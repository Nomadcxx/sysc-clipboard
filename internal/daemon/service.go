// Package daemon contains the clipboard reducer and its local client service.
package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/internal/store"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
	_ "image/gif"
	_ "image/jpeg"
)

var (
	ErrServiceClosed        = errors.New("clipboard service is closed")
	ErrRestoreUnavailable   = errors.New("clipboard restore is unavailable")
	ErrInvalidWaylandState  = errors.New("invalid clipboard Wayland state")
	ErrThumbnailUnsupported = errors.New("clipboard thumbnail is unsupported")
	ErrThumbnailLimit       = errors.New("clipboard thumbnail exceeds a limit")
)

const (
	maxDecodedPixels = 16 << 20
	serviceQueueSize = 4
	// ponytail: one active and one queued decode bounds image work; raise only after measuring peak decode memory.
	thumbnailQueueSize       = 1
	restoreTimeout           = 5 * time.Second
	maxThumbnailCacheBytes   = 8 << 20
	maxThumbnailCacheEntries = 64
)

// Options supplies the state owned by the reducer. History must not be used
// by another goroutine after the service is constructed.
type Options struct {
	History     *history.History
	Store       *store.Store
	Persistence protocol.PersistenceState
	Wayland     protocol.WaylandState
	// Restore runs outside the reducer and must return when its context is canceled.
	Restore func(context.Context, history.Item) error
}

// Service serializes all history changes, revisions, and publications on one
// goroutine.
type Service struct {
	requests chan serviceRequest
	stop     chan struct{}
	done     chan struct{}
	close    sync.Once

	ctx              context.Context
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	thumbnailJobs    chan thumbnailJob
	thumbnailResults chan thumbnailResult
	restoreJobs      chan restoreJob
	restoreResults   chan restoreResult
	thumbnailize     func(history.Item, uint16) (protocol.Thumbnail, error)
	restore          func(context.Context, history.Item) error
}

type requestKind uint8

const (
	requestCapture requestKind = iota + 1
	requestCommand
	requestSubscribe
	requestUnsubscribe
	requestSnapshot
	requestWayland
	requestGeneration
)

type serviceRequest struct {
	kind       requestKind
	ctx        context.Context
	capture    history.Capture
	message    protocol.Message
	wayland    protocol.WaylandState
	generation uint64
	sub        *Subscription
	reply      chan serviceReply
}

type serviceReply struct {
	err      error
	message  protocol.Message
	snapshot protocol.Snapshot
	sub      *Subscription
}

type thumbnailJob struct {
	ctx       context.Context
	requestID string
	maxPX     uint16
	identity  protocol.Entry
	item      history.Item
	reply     chan serviceReply
}

type thumbnailResult struct {
	ctx       context.Context
	requestID string
	maxPX     uint16
	identity  protocol.Entry
	thumbnail protocol.Thumbnail
	err       error
	reply     chan serviceReply
}

type restoreJob struct {
	id        uint64
	ctx       context.Context
	cancel    context.CancelFunc
	requestID string
	item      history.Item
	reply     chan serviceReply
}

type restoreResult struct {
	id        uint64
	ctx       context.Context
	requestID string
	err       error
	reply     chan serviceReply
}

type pendingRestore struct {
	itemID string
	cancel context.CancelFunc
}

// Subscription receives metadata-only snapshots and deltas. It has at most
// one pending message; a slow reader receives a current snapshot instead of
// an unbounded backlog.
type Subscription struct {
	Updates <-chan protocol.Message

	service *Service
	id      uint64
	updates chan protocol.Message
	close   sync.Once
}

// NewService starts the reducer immediately.
func NewService(options Options) *Service {
	if options.History == nil {
		options.History = history.New(0)
	}
	if options.Wayland != protocol.WaylandReady {
		options.Wayland = protocol.WaylandUnavailable
	}
	if options.Store == nil {
		options.Persistence = protocol.PersistenceUnavailable
	} else if options.Persistence != protocol.PersistenceVolatile {
		options.Persistence = protocol.PersistenceDurable
	}

	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		requests:         make(chan serviceRequest, serviceQueueSize),
		stop:             make(chan struct{}),
		done:             make(chan struct{}),
		ctx:              ctx,
		cancel:           cancel,
		thumbnailJobs:    make(chan thumbnailJob, thumbnailQueueSize),
		thumbnailResults: make(chan thumbnailResult, 1),
		restoreJobs:      make(chan restoreJob, serviceQueueSize),
		restoreResults:   make(chan restoreResult, 1),
		thumbnailize:     makeThumbnail,
		restore:          options.Restore,
	}
	service.workers.Add(2)
	go service.runThumbnailWorker()
	go service.runRestoreWorker()
	go service.run(options)
	return service
}

// Close stops the reducer and closes all subscriber streams.
func (service *Service) Close() {
	service.close.Do(func() {
		close(service.stop)
		service.cancel()
	})
	<-service.done
	service.workers.Wait()
}

// Capture submits a completed Wayland offer read to the reducer.
func (service *Service) Capture(ctx context.Context, capture history.Capture) error {
	capture.Payload = bytes.Clone(capture.Payload)
	capture.OfferedMIME = append([]string(nil), capture.OfferedMIME...)
	return service.submit(ctx, serviceRequest{
		kind:    requestCapture,
		capture: capture,
		reply:   make(chan serviceReply, 1),
	})
}

// Execute handles one validated client command and returns its acknowledgement,
// snapshot, thumbnail, or structured error.
func (service *Service) Execute(ctx context.Context, message protocol.Message) protocol.Message {
	if err := protocol.ValidateMessage(message); err != nil {
		return errorMessage("", protocol.ErrorProtocol, "invalid clipboard command")
	}
	message = cloneMessage(message)
	reply := make(chan serviceReply, 1)
	request := serviceRequest{kind: requestCommand, ctx: ctx, message: message, reply: reply}
	if err := service.submitRequest(ctx, request); err != nil {
		return errorMessage(message.RequestID, protocol.ErrorUnavailable, err.Error())
	}
	select {
	case result := <-reply:
		if result.err != nil {
			return errorMessage(message.RequestID, protocol.ErrorUnavailable, result.err.Error())
		}
		return result.message
	case <-ctx.Done():
		return errorMessage(message.RequestID, protocol.ErrorUnavailable, "clipboard command timed out")
	case <-service.done:
		return errorMessage(message.RequestID, protocol.ErrorUnavailable, ErrServiceClosed.Error())
	}
}

// Subscribe registers a bounded metadata stream and returns its initial
// snapshot as the first update.
func (service *Service) Subscribe(ctx context.Context) (*Subscription, error) {
	reply := make(chan serviceReply, 1)
	if err := service.submitRequest(ctx, serviceRequest{kind: requestSubscribe, reply: reply}); err != nil {
		return nil, err
	}
	select {
	case result := <-reply:
		return result.sub, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-service.done:
		return nil, ErrServiceClosed
	}
}

// CurrentSnapshot returns an immutable copy of current metadata and status.
func (service *Service) CurrentSnapshot(ctx context.Context) (protocol.Snapshot, error) {
	reply := make(chan serviceReply, 1)
	if err := service.submitRequest(ctx, serviceRequest{kind: requestSnapshot, reply: reply}); err != nil {
		return protocol.Snapshot{}, err
	}
	select {
	case result := <-reply:
		return result.snapshot, result.err
	case <-ctx.Done():
		return protocol.Snapshot{}, ctx.Err()
	case <-service.done:
		return protocol.Snapshot{}, ErrServiceClosed
	}
}

// SetWaylandState publishes a capability change without advancing the
// history revision.
func (service *Service) SetWaylandState(ctx context.Context, wayland protocol.WaylandState) error {
	reply := make(chan serviceReply, 1)
	if err := service.submitRequest(ctx, serviceRequest{kind: requestWayland, wayland: wayland, reply: reply}); err != nil {
		return err
	}
	select {
	case result := <-reply:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-service.done:
		return ErrServiceClosed
	}
}

// SetGeneration advances the capture generation without publishing a history
// revision. Wayland offer workers use it to reject results from an older
// selection.
func (service *Service) SetGeneration(ctx context.Context, generation uint64) error {
	reply := make(chan serviceReply, 1)
	if err := service.submitRequest(ctx, serviceRequest{kind: requestGeneration, generation: generation, reply: reply}); err != nil {
		return err
	}
	select {
	case result := <-reply:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-service.done:
		return ErrServiceClosed
	}
}

func (subscription *Subscription) Close() {
	if subscription == nil {
		return
	}
	subscription.close.Do(func() {
		if subscription.service == nil {
			return
		}
		request := serviceRequest{kind: requestUnsubscribe, sub: subscription, reply: make(chan serviceReply, 1)}
		_ = subscription.service.submitRequest(context.Background(), request)
	})
}

func (service *Service) submit(ctx context.Context, request serviceRequest) error {
	if err := service.submitRequest(ctx, request); err != nil {
		return err
	}
	select {
	case result := <-request.reply:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-service.done:
		return ErrServiceClosed
	}
}

func (service *Service) submitRequest(ctx context.Context, request serviceRequest) error {
	select {
	case service.requests <- request:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-service.done:
		return ErrServiceClosed
	}
}

func (service *Service) runThumbnailWorker() {
	defer service.workers.Done()
	for {
		select {
		case <-service.ctx.Done():
			return
		case job, ok := <-service.thumbnailJobs:
			if !ok || service.ctx.Err() != nil {
				return
			}
			if job.ctx.Err() != nil {
				continue
			}
			thumbnail, err := service.thumbnailize(job.item, job.maxPX)
			if job.ctx.Err() != nil {
				continue
			}
			result := thumbnailResult{
				ctx: job.ctx, requestID: job.requestID, maxPX: job.maxPX,
				identity: job.identity, thumbnail: thumbnail, err: err, reply: job.reply,
			}
			select {
			case service.thumbnailResults <- result:
			case <-service.ctx.Done():
				return
			case <-job.ctx.Done():
			}
		}
	}
}

func (service *Service) runRestoreWorker() {
	defer service.workers.Done()
	for {
		select {
		case <-service.ctx.Done():
			return
		case job, ok := <-service.restoreJobs:
			if !ok || service.ctx.Err() != nil {
				return
			}
			err := job.ctx.Err()
			if err == nil {
				stop := context.AfterFunc(service.ctx, job.cancel)
				err = service.restore(job.ctx, job.item)
				stop()
				if err == nil {
					err = job.ctx.Err()
				}
			}
			result := restoreResult{
				id: job.id, ctx: job.ctx, requestID: job.requestID, err: err, reply: job.reply,
			}
			select {
			case service.restoreResults <- result:
			case <-service.ctx.Done():
				return
			}
		}
	}
}

func (service *Service) run(options Options) {
	defer func() {
		close(service.thumbnailJobs)
		close(service.restoreJobs)
		close(service.done)
	}()
	state := reducerState{
		history:         options.History,
		store:           options.Store,
		persistence:     options.Persistence,
		wayland:         options.Wayland,
		subscribers:     make(map[uint64]*Subscription),
		thumbnails:      newThumbnailCache(),
		pendingRestores: make(map[uint64]pendingRestore),
	}

	for {
		select {
		case <-service.stop:
			for _, pending := range state.pendingRestores {
				pending.cancel()
			}
			for _, subscription := range state.subscribers {
				close(subscription.updates)
			}
			return
		case request := <-service.requests:
			switch request.kind {
			case requestCapture:
				request.reply <- serviceReply{err: state.capture(request.capture)}
			case requestCommand:
				switch request.message.Type {
				case protocol.TypeThumbnail:
					state.requestThumbnail(service, request)
				case protocol.TypeRestore:
					state.requestRestore(service, request)
				default:
					request.reply <- serviceReply{message: state.execute(request.message)}
				}
			case requestSnapshot:
				request.reply <- serviceReply{snapshot: state.snapshot()}
			case requestWayland:
				request.reply <- serviceReply{err: state.setWayland(request.wayland)}
			case requestGeneration:
				state.history.SetGeneration(request.generation)
				request.reply <- serviceReply{}
			case requestSubscribe:
				state.nextSubscriptionID++
				updates := make(chan protocol.Message, 1)
				subscription := &Subscription{service: service, id: state.nextSubscriptionID, Updates: updates, updates: updates}
				state.subscribers[subscription.id] = subscription
				updates <- state.snapshotMessage()
				request.reply <- serviceReply{sub: subscription}
			case requestUnsubscribe:
				if subscription, ok := state.subscribers[request.sub.id]; ok {
					delete(state.subscribers, request.sub.id)
					close(subscription.updates)
				}
				request.reply <- serviceReply{}
			}
		case result := <-service.thumbnailResults:
			state.completeThumbnail(result)
		case result := <-service.restoreResults:
			state.completeRestore(result)
		}
	}
}

type reducerState struct {
	history            *history.History
	store              *store.Store
	persistence        protocol.PersistenceState
	wayland            protocol.WaylandState
	revision           uint64
	subscribers        map[uint64]*Subscription
	thumbnails         *thumbnailCache
	nextSubscriptionID uint64
	nextRestoreID      uint64
	pendingRestores    map[uint64]pendingRestore
}

func (state *reducerState) capture(capture history.Capture) error {
	changes, err := state.history.Capture(capture)
	if err != nil {
		return err
	}
	state.commit(changes)
	return nil
}

func (state *reducerState) requestRestore(service *Service, request serviceRequest) {
	message := request.message
	if service.restore == nil {
		request.reply <- serviceReply{message: errorMessage(message.RequestID, protocol.ErrorUnavailable, ErrRestoreUnavailable.Error())}
		return
	}
	item, ok := state.history.ItemView(message.ID)
	if !ok {
		request.reply <- serviceReply{message: errorMessage(message.RequestID, protocol.ErrorNotFound, history.ErrNotFound.Error())}
		return
	}
	ctx := request.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return
	}
	state.nextRestoreID++
	id := state.nextRestoreID
	jobCtx, cancel := context.WithTimeout(ctx, restoreTimeout)
	state.pendingRestores[id] = pendingRestore{itemID: message.ID, cancel: cancel}
	job := restoreJob{id: id, ctx: jobCtx, cancel: cancel, requestID: message.RequestID, item: item, reply: request.reply}
	select {
	case service.restoreJobs <- job:
	default:
		delete(state.pendingRestores, id)
		cancel()
		request.reply <- serviceReply{message: errorMessage(message.RequestID, protocol.ErrorUnavailable, "clipboard restore worker is busy")}
	}
}

func (state *reducerState) requestThumbnail(service *Service, request serviceRequest) {
	message := request.message
	identity, ok := state.history.Metadata(message.ID)
	if !ok {
		request.reply <- serviceReply{message: thumbnailError(message.RequestID, history.ErrNotFound)}
		return
	}
	key := thumbnailKey{id: message.ID, maxPX: message.MaxPX}
	if thumbnail, ok := state.thumbnails.get(key, identity); ok {
		request.reply <- serviceReply{message: thumbnailMessage(message.RequestID, thumbnail)}
		return
	}
	item, ok := state.history.ItemView(message.ID)
	if !ok {
		request.reply <- serviceReply{message: thumbnailError(message.RequestID, history.ErrNotFound)}
		return
	}
	ctx := request.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	job := thumbnailJob{
		ctx: ctx, requestID: message.RequestID, maxPX: message.MaxPX,
		identity: identity, item: item, reply: request.reply,
	}
	select {
	case service.thumbnailJobs <- job:
	default:
		request.reply <- serviceReply{message: errorMessage(message.RequestID, protocol.ErrorUnavailable, "clipboard thumbnail worker is busy")}
	}
}

func (state *reducerState) completeThumbnail(result thumbnailResult) {
	if result.ctx.Err() != nil {
		return
	}
	current, ok := state.history.Metadata(result.identity.ID)
	if !ok || !sameThumbnailIdentity(current, result.identity) {
		result.reply <- serviceReply{message: thumbnailError(result.requestID, history.ErrNotFound)}
		return
	}
	if result.err != nil {
		result.reply <- serviceReply{message: thumbnailError(result.requestID, result.err)}
		return
	}
	key := thumbnailKey{id: result.identity.ID, maxPX: result.maxPX}
	state.thumbnails.put(key, result.identity, result.thumbnail)
	result.reply <- serviceReply{message: thumbnailMessage(result.requestID, result.thumbnail)}
}

func (state *reducerState) completeRestore(result restoreResult) {
	pending, ok := state.pendingRestores[result.id]
	if !ok {
		return
	}
	delete(state.pendingRestores, result.id)
	if result.err == nil {
		result.err = result.ctx.Err()
	}
	pending.cancel()
	if result.err != nil {
		result.reply <- serviceReply{message: errorMessage(result.requestID, protocol.ErrorUnavailable, boundedError(result.err))}
		return
	}
	result.reply <- serviceReply{message: acknowledgement(result.requestID, state.revision)}
}

func (state *reducerState) cancelPendingRestores(itemID string) {
	for _, pending := range state.pendingRestores {
		if pending.itemID == itemID {
			pending.cancel()
		}
	}
}

func (state *reducerState) execute(message protocol.Message) protocol.Message {
	switch message.Type {
	case protocol.TypePin:
		changes, err := state.history.SetPinned(message.ID, *message.Pinned)
		if err != nil {
			return historyError(message.RequestID, err)
		}
		state.commit(changes)
		return acknowledgement(message.RequestID, state.revision)
	case protocol.TypeDelete:
		changes, err := state.history.Delete(message.ID)
		if err != nil {
			return historyError(message.RequestID, err)
		}
		state.commit(changes)
		return acknowledgement(message.RequestID, state.revision)
	case protocol.TypeClear:
		changes, err := state.history.Clear(message.Scope)
		if err != nil {
			return errorMessage(message.RequestID, protocol.ErrorProtocol, err.Error())
		}
		state.commit(changes)
		return acknowledgement(message.RequestID, state.revision)
	case protocol.TypeResync:
		response := state.snapshotMessage()
		response.RequestID = message.RequestID
		return response
	default:
		return errorMessage(message.RequestID, protocol.ErrorUnsupported, "unsupported clipboard command")
	}
}

func (state *reducerState) commit(changes []protocol.Change) {
	if len(changes) == 0 {
		return
	}
	for _, change := range changes {
		if change.Kind == protocol.ChangeRemoved {
			state.thumbnails.removeID(change.ID)
			state.cancelPendingRestores(change.ID)
		}
	}
	state.revision++
	if state.store == nil {
		state.persistence = protocol.PersistenceUnavailable
	} else if err := state.store.Save(state.history.Items()); err != nil {
		state.persistence = protocol.PersistenceVolatile
	} else {
		state.persistence = protocol.PersistenceDurable
	}
	message := protocol.Message{
		Version: protocol.Version,
		Type:    protocol.TypeDelta,
		Delta: &protocol.Delta{
			Revision:    state.revision,
			Changes:     cloneChanges(changes),
			Persistence: state.persistence,
			Wayland:     state.wayland,
		},
	}
	for _, subscription := range state.subscribers {
		state.publish(subscription, message)
	}
}

func (state *reducerState) setWayland(wayland protocol.WaylandState) error {
	if wayland != protocol.WaylandReady && wayland != protocol.WaylandUnavailable {
		return ErrInvalidWaylandState
	}
	if state.wayland == wayland {
		return nil
	}
	state.wayland = wayland
	message := state.snapshotMessage()
	for _, subscription := range state.subscribers {
		state.publish(subscription, message)
	}
	return nil
}

func (state *reducerState) publish(subscription *Subscription, message protocol.Message) {
	message = cloneMessage(message)
	select {
	case subscription.updates <- message:
		return
	default:
	}
	select {
	case <-subscription.updates:
	default:
	}
	select {
	case subscription.updates <- state.snapshotMessage():
	default:
	}
}

func (state *reducerState) snapshot() protocol.Snapshot {
	return protocol.Snapshot{
		Revision:    state.revision,
		Entries:     state.history.Snapshot(),
		Persistence: state.persistence,
		Wayland:     state.wayland,
	}
}

func (state *reducerState) snapshotMessage() protocol.Message {
	snapshot := state.snapshot()
	return protocol.Message{Version: protocol.Version, Type: protocol.TypeSnapshot, Snapshot: &snapshot}
}

func acknowledgement(requestID string, revision uint64) protocol.Message {
	return protocol.Message{Version: protocol.Version, Type: protocol.TypeAck, Ack: &protocol.Ack{RequestID: requestID, Revision: revision}}
}

func historyError(requestID string, err error) protocol.Message {
	switch {
	case errors.Is(err, history.ErrNotFound):
		return errorMessage(requestID, protocol.ErrorNotFound, err.Error())
	case errors.Is(err, history.ErrLimit):
		return errorMessage(requestID, protocol.ErrorLimit, err.Error())
	default:
		return errorMessage(requestID, protocol.ErrorProtocol, boundedError(err))
	}
}

func thumbnailError(requestID string, err error) protocol.Message {
	switch {
	case errors.Is(err, history.ErrNotFound):
		return errorMessage(requestID, protocol.ErrorNotFound, err.Error())
	case errors.Is(err, ErrThumbnailLimit):
		return errorMessage(requestID, protocol.ErrorLimit, err.Error())
	case errors.Is(err, ErrThumbnailUnsupported):
		return errorMessage(requestID, protocol.ErrorUnsupported, err.Error())
	default:
		return errorMessage(requestID, protocol.ErrorUnsupported, boundedError(err))
	}
}

func errorMessage(requestID string, code protocol.ErrorCode, message string) protocol.Message {
	return protocol.Message{
		Version: protocol.Version,
		Type:    protocol.TypeError,
		Error:   &protocol.ErrorBody{Code: code, Message: boundedErrorString(message), RequestID: requestID},
	}
}

func boundedError(err error) string {
	if err == nil {
		return "clipboard operation failed"
	}
	return boundedErrorString(err.Error())
}

func boundedErrorString(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len([]byte(message)) <= protocol.MaxPreviewBytes {
		return message
	}
	cut := protocol.MaxPreviewBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return message[:cut]
}

func cloneChanges(changes []protocol.Change) []protocol.Change {
	cloned := make([]protocol.Change, len(changes))
	for index, change := range changes {
		cloned[index] = change
		if change.Entry != nil {
			entry := *change.Entry
			entry.OfferedMIME = append([]string(nil), change.Entry.OfferedMIME...)
			cloned[index].Entry = &entry
		}
	}
	return cloned
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
		snapshot := *message.Snapshot
		snapshot.Entries = append([]protocol.Entry(nil), message.Snapshot.Entries...)
		for index := range snapshot.Entries {
			snapshot.Entries[index].OfferedMIME = append([]string(nil), snapshot.Entries[index].OfferedMIME...)
		}
		cloned.Snapshot = &snapshot
	}
	if message.Delta != nil {
		delta := *message.Delta
		delta.Changes = cloneChanges(message.Delta.Changes)
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
		thumbnail.Data = bytes.Clone(message.Thumbnail.Data)
		cloned.Thumbnail = &thumbnail
	}
	return cloned
}

type thumbnailKey struct {
	id    string
	maxPX uint16
}

type thumbnailCacheEntry struct {
	identity  protocol.Entry
	thumbnail protocol.Thumbnail
}

type thumbnailCache struct {
	entries map[thumbnailKey]thumbnailCacheEntry
	order   []thumbnailKey
	bytes   int
}

func newThumbnailCache() *thumbnailCache {
	return &thumbnailCache{entries: make(map[thumbnailKey]thumbnailCacheEntry)}
}

func (cache *thumbnailCache) get(key thumbnailKey, identity protocol.Entry) (protocol.Thumbnail, bool) {
	entry, ok := cache.entries[key]
	if !ok {
		return protocol.Thumbnail{}, false
	}
	if !sameThumbnailIdentity(entry.identity, identity) {
		cache.remove(key)
		return protocol.Thumbnail{}, false
	}
	thumbnail := entry.thumbnail
	thumbnail.Data = bytes.Clone(thumbnail.Data)
	return thumbnail, true
}

func (cache *thumbnailCache) put(key thumbnailKey, identity protocol.Entry, thumbnail protocol.Thumbnail) {
	if len(thumbnail.Data) == 0 || len(thumbnail.Data) > maxThumbnailCacheBytes {
		return
	}
	cache.remove(key)
	for len(cache.entries) >= maxThumbnailCacheEntries || cache.bytes+len(thumbnail.Data) > maxThumbnailCacheBytes {
		cache.evictOldest()
	}
	stored := thumbnail
	stored.Data = bytes.Clone(thumbnail.Data)
	cache.entries[key] = thumbnailCacheEntry{identity: identity, thumbnail: stored}
	cache.order = append(cache.order, key)
	cache.bytes += len(stored.Data)
}

func (cache *thumbnailCache) removeID(id string) {
	for key := range cache.entries {
		if key.id == id {
			cache.remove(key)
		}
	}
}

func (cache *thumbnailCache) evictOldest() {
	if len(cache.order) > 0 {
		cache.remove(cache.order[0])
	}
}

func (cache *thumbnailCache) remove(key thumbnailKey) {
	entry, ok := cache.entries[key]
	if !ok {
		return
	}
	delete(cache.entries, key)
	cache.bytes -= len(entry.thumbnail.Data)
	for index, queued := range cache.order {
		if queued == key {
			copy(cache.order[index:], cache.order[index+1:])
			cache.order = cache.order[:len(cache.order)-1]
			return
		}
	}
}

func sameThumbnailIdentity(left, right protocol.Entry) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.MIME == right.MIME &&
		left.Size == right.Size && left.SHA256 == right.SHA256
}

func thumbnailMessage(requestID string, thumbnail protocol.Thumbnail) protocol.Message {
	return protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: requestID, Thumbnail: &thumbnail}
}

func makeThumbnail(item history.Item, maxPX uint16) (protocol.Thumbnail, error) {
	metadata := item.Metadata()
	if metadata.Kind != protocol.KindImage {
		return protocol.Thumbnail{}, ErrThumbnailUnsupported
	}
	switch metadata.MIME {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return protocol.Thumbnail{}, ErrThumbnailUnsupported
	}
	config, _, err := image.DecodeConfig(item.PayloadReader())
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return protocol.Thumbnail{}, ErrThumbnailUnsupported
	}
	if int64(config.Width)*int64(config.Height) > maxDecodedPixels {
		return protocol.Thumbnail{}, ErrThumbnailLimit
	}
	decoded, _, err := image.Decode(item.PayloadReader())
	if err != nil {
		return protocol.Thumbnail{}, ErrThumbnailUnsupported
	}
	width, height := boundedDimensions(decoded.Bounds().Dx(), decoded.Bounds().Dy(), int(maxPX))
	thumbnailImage := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sourceY := y * decoded.Bounds().Dy() / height
		for x := 0; x < width; x++ {
			sourceX := x * decoded.Bounds().Dx() / width
			thumbnailImage.Set(x, y, decoded.At(decoded.Bounds().Min.X+sourceX, decoded.Bounds().Min.Y+sourceY))
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, thumbnailImage); err != nil {
		return protocol.Thumbnail{}, fmt.Errorf("encode clipboard thumbnail: %w", err)
	}
	if encoded.Len() == 0 || encoded.Len() > protocol.MaxThumbnailBytes {
		return protocol.Thumbnail{}, ErrThumbnailLimit
	}
	return protocol.Thumbnail{ID: metadata.ID, MIME: "image/png", Width: uint16(width), Height: uint16(height), Data: encoded.Bytes()}, nil
}

func boundedDimensions(width, height, maxPX int) (int, int) {
	if maxPX <= 0 {
		maxPX = 1
	}
	if width <= maxPX && height <= maxPX {
		return width, height
	}
	if width >= height {
		return maxPX, max(1, height*maxPX/width)
	}
	return max(1, width*maxPX/height), maxPX
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

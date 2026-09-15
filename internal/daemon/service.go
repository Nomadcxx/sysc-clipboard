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
)

// Options supplies the state owned by the reducer. History must not be used
// by another goroutine after the service is constructed.
type Options struct {
	History     *history.History
	Store       *store.Store
	Persistence protocol.PersistenceState
	Wayland     protocol.WaylandState
	Restore     func(history.Item) error
}

// Service serializes all history changes, revisions, and publications on one
// goroutine.
type Service struct {
	requests chan serviceRequest
	stop     chan struct{}
	done     chan struct{}
	close    sync.Once
}

type requestKind uint8

const (
	requestCapture requestKind = iota + 1
	requestCommand
	requestSubscribe
	requestUnsubscribe
	requestSnapshot
	requestWayland
)

type serviceRequest struct {
	kind    requestKind
	capture history.Capture
	message protocol.Message
	wayland protocol.WaylandState
	sub     *Subscription
	reply   chan serviceReply
}

type serviceReply struct {
	err      error
	message  protocol.Message
	snapshot protocol.Snapshot
	sub      *Subscription
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

	service := &Service{
		requests: make(chan serviceRequest, serviceQueueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go service.run(options)
	return service
}

// Close stops the reducer and closes all subscriber streams.
func (service *Service) Close() {
	service.close.Do(func() { close(service.stop) })
	<-service.done
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
	request := serviceRequest{kind: requestCommand, message: message, reply: reply}
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

func (service *Service) run(options Options) {
	defer close(service.done)
	state := reducerState{
		history:     options.History,
		store:       options.Store,
		persistence: options.Persistence,
		wayland:     options.Wayland,
		restore:     options.Restore,
		subscribers: make(map[uint64]*Subscription),
	}
	var nextSubscriptionID uint64

	for {
		select {
		case <-service.stop:
			for _, subscription := range state.subscribers {
				close(subscription.updates)
			}
			return
		case request := <-service.requests:
			switch request.kind {
			case requestCapture:
				request.reply <- serviceReply{err: state.capture(request.capture)}
			case requestCommand:
				request.reply <- serviceReply{message: state.execute(request.message)}
			case requestSnapshot:
				request.reply <- serviceReply{snapshot: state.snapshot()}
			case requestWayland:
				request.reply <- serviceReply{err: state.setWayland(request.wayland)}
			case requestSubscribe:
				nextSubscriptionID++
				updates := make(chan protocol.Message, 1)
				subscription := &Subscription{service: service, id: nextSubscriptionID, Updates: updates, updates: updates}
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
		}
	}
}

type reducerState struct {
	history     *history.History
	store       *store.Store
	persistence protocol.PersistenceState
	wayland     protocol.WaylandState
	revision    uint64
	restore     func(history.Item) error
	subscribers map[uint64]*Subscription
}

func (state *reducerState) capture(capture history.Capture) error {
	changes, err := state.history.Capture(capture)
	if err != nil {
		return err
	}
	state.commit(changes)
	return nil
}

func (state *reducerState) execute(message protocol.Message) protocol.Message {
	switch message.Type {
	case protocol.TypeRestore:
		item, ok := state.history.Item(message.ID)
		if !ok {
			return errorMessage(message.RequestID, protocol.ErrorNotFound, history.ErrNotFound.Error())
		}
		if state.restore == nil {
			return errorMessage(message.RequestID, protocol.ErrorUnavailable, ErrRestoreUnavailable.Error())
		}
		if err := state.restore(item); err != nil {
			return errorMessage(message.RequestID, protocol.ErrorUnavailable, boundedError(err))
		}
		return acknowledgement(message.RequestID, state.revision)
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
	case protocol.TypeThumbnail:
		item, ok := state.history.Item(message.ID)
		if !ok {
			return errorMessage(message.RequestID, protocol.ErrorNotFound, history.ErrNotFound.Error())
		}
		thumbnail, err := makeThumbnail(item, message.MaxPX)
		if err != nil {
			return thumbnailError(message.RequestID, err)
		}
		return protocol.Message{Version: protocol.Version, Type: protocol.TypeThumbnail, RequestID: message.RequestID, Thumbnail: &thumbnail}
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
	payload := item.Payload()
	config, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return protocol.Thumbnail{}, ErrThumbnailUnsupported
	}
	if int64(config.Width)*int64(config.Height) > maxDecodedPixels {
		return protocol.Thumbnail{}, ErrThumbnailLimit
	}
	decoded, _, err := image.Decode(bytes.NewReader(payload))
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

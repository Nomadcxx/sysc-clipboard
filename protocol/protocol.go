// Package protocol defines the bounded, metadata-only sysc-clipboard wire
// protocol.
package protocol

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version = 1

	MaxFrame        = 1 << 20
	MaxJSONDepth    = 32
	MaxIDBytes      = 128
	MaxMIMEBytes    = 256
	MaxPreviewBytes = 200
	MaxOfferedMIME  = 32
	MaxEntries      = 100
	MaxTotalBytes   = 256 << 20
	MaxTextBytes    = 4 << 20
	MaxImageBytes   = 32 << 20

	MaxCapabilities    = 32
	MaxCapabilityBytes = 64
	MaxChanges         = 256
	MaxThumbnailPixels = 512
	MaxThumbnailBytes  = 512 << 10
)

type MessageType string

const (
	TypeHello     MessageType = "hello"
	TypeSnapshot  MessageType = "snapshot"
	TypeDelta     MessageType = "delta"
	TypeAck       MessageType = "ack"
	TypeError     MessageType = "error"
	TypeRestore   MessageType = "restore"
	TypePin       MessageType = "pin"
	TypeDelete    MessageType = "delete"
	TypeClear     MessageType = "clear"
	TypeThumbnail MessageType = "thumbnail"
	TypeResync    MessageType = "resync"
)

type Kind string

const (
	KindText  Kind = "text"
	KindImage Kind = "image"
)

type PersistenceState string

const (
	PersistenceDurable     PersistenceState = "durable"
	PersistenceVolatile    PersistenceState = "volatile"
	PersistenceUnavailable PersistenceState = "unavailable"
)

type WaylandState string

const (
	WaylandReady       WaylandState = "ready"
	WaylandUnavailable WaylandState = "unavailable"
)

type ClearScope string

const (
	ClearUnpinned ClearScope = "unpinned"
	ClearAll      ClearScope = "all"
)

type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"
	ChangeMoved   ChangeKind = "moved"
	ChangeUpdated ChangeKind = "updated"
	ChangeRemoved ChangeKind = "removed"
)

type ErrorCode string

const (
	ErrorUnsupported ErrorCode = "unsupported"
	ErrorNotFound    ErrorCode = "not_found"
	ErrorLimit       ErrorCode = "limit"
	ErrorUnavailable ErrorCode = "unavailable"
	ErrorPersistence ErrorCode = "persistence"
	ErrorProtocol    ErrorCode = "protocol"
	ErrorConflict    ErrorCode = "conflict"
)

// Entry contains metadata only. Payload bytes never cross the client
// protocol.
type Entry struct {
	ID          string    `json:"id"`
	Kind        Kind      `json:"kind"`
	MIME        string    `json:"mime"`
	OfferedMIME []string  `json:"offered_mime"`
	Size        uint64    `json:"size"`
	SHA256      string    `json:"sha256"`
	CapturedAt  time.Time `json:"captured_at"`
	Preview     string    `json:"preview,omitempty"`
	Pinned      bool      `json:"pinned"`
}

type Snapshot struct {
	Revision    uint64           `json:"revision"`
	Entries     []Entry          `json:"entries"`
	Persistence PersistenceState `json:"persistence"`
	Wayland     WaylandState     `json:"wayland"`
}

type Change struct {
	Kind  ChangeKind `json:"kind"`
	ID    string     `json:"id"`
	Entry *Entry     `json:"entry,omitempty"`
}

type Delta struct {
	Revision    uint64           `json:"revision"`
	Changes     []Change         `json:"changes"`
	Persistence PersistenceState `json:"persistence,omitempty"`
	Wayland     WaylandState     `json:"wayland,omitempty"`
}

type Hello struct {
	Capabilities []string         `json:"capabilities,omitempty"`
	Persistence  PersistenceState `json:"persistence,omitempty"`
	Wayland      WaylandState     `json:"wayland,omitempty"`
}

type Ack struct {
	RequestID string `json:"request_id,omitempty"`
	Revision  uint64 `json:"revision"`
}

type ErrorBody struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id,omitempty"`
}

type Thumbnail struct {
	ID     string `json:"id"`
	MIME   string `json:"mime"`
	Width  uint16 `json:"width"`
	Height uint16 `json:"height"`
	Data   []byte `json:"data"`
}

// Message is the single JSON object carried by a frame. The Type field
// selects exactly one of the body forms or command fields below.
type Message struct {
	Version   int         `json:"version"`
	Type      MessageType `json:"type"`
	RequestID string      `json:"request_id,omitempty"`

	Hello     *Hello     `json:"hello,omitempty"`
	Snapshot  *Snapshot  `json:"snapshot,omitempty"`
	Delta     *Delta     `json:"delta,omitempty"`
	Ack       *Ack       `json:"ack,omitempty"`
	Error     *ErrorBody `json:"error,omitempty"`
	Thumbnail *Thumbnail `json:"thumbnail,omitempty"`

	ID     string     `json:"id,omitempty"`
	Pinned *bool      `json:"pinned,omitempty"`
	Scope  ClearScope `json:"scope,omitempty"`
	MaxPX  uint16     `json:"max_px,omitempty"`
}

func ValidateMessage(message Message) error {
	if message.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", message.Version)
	}
	if !validMessageType(message.Type) {
		return fmt.Errorf("unknown message type %q", message.Type)
	}
	if err := validateOptionalID(message.RequestID, "request_id"); err != nil {
		return err
	}

	switch message.Type {
	case TypeHello:
		if err := validateHello(message.Hello); err != nil {
			return err
		}
		return noCommandFields(message, false, false, false, false)
	case TypeSnapshot:
		if message.Snapshot == nil {
			return fmt.Errorf("snapshot message has no snapshot body")
		}
		if err := ValidateSnapshot(*message.Snapshot); err != nil {
			return err
		}
		return noCommandFields(message, false, false, false, false)
	case TypeDelta:
		if message.Delta == nil {
			return fmt.Errorf("delta message has no delta body")
		}
		if err := ValidateDelta(*message.Delta); err != nil {
			return err
		}
		return noCommandFields(message, false, false, false, false)
	case TypeAck:
		if message.Ack == nil {
			return fmt.Errorf("ack message has no ack body")
		}
		if err := validateAck(*message.Ack); err != nil {
			return err
		}
		return noCommandFields(message, false, false, false, false)
	case TypeError:
		if message.Error == nil {
			return fmt.Errorf("error message has no error body")
		}
		if err := validateError(*message.Error); err != nil {
			return err
		}
		return noCommandFields(message, false, false, false, false)
	case TypeRestore, TypeDelete:
		if err := validateRequiredID(message.ID); err != nil {
			return err
		}
		return noCommandFields(message, true, false, false, false)
	case TypePin:
		if err := validateRequiredID(message.ID); err != nil {
			return err
		}
		if message.Pinned == nil {
			return fmt.Errorf("pin message has no pinned value")
		}
		return noCommandFields(message, true, true, false, false)
	case TypeClear:
		if message.Scope != ClearUnpinned && message.Scope != ClearAll {
			return fmt.Errorf("invalid clear scope %q", message.Scope)
		}
		return noCommandFields(message, false, false, true, false)
	case TypeThumbnail:
		if message.Thumbnail != nil {
			if err := ValidateThumbnail(*message.Thumbnail); err != nil {
				return err
			}
			return noCommandFields(message, false, false, false, false)
		}
		if err := validateRequiredID(message.ID); err != nil {
			return err
		}
		if message.MaxPX == 0 || int(message.MaxPX) > MaxThumbnailPixels {
			return fmt.Errorf("invalid thumbnail size %d", message.MaxPX)
		}
		return noCommandFields(message, true, false, false, true)
	case TypeResync:
		return noCommandFields(message, false, false, false, false)
	default:
		panic("validated message type missing switch case")
	}
}

func ValidateSnapshot(snapshot Snapshot) error {
	if len(snapshot.Entries) > MaxEntries {
		return fmt.Errorf("snapshot has %d entries, limit is %d", len(snapshot.Entries), MaxEntries)
	}
	if !validPersistence(snapshot.Persistence) {
		return fmt.Errorf("invalid persistence state %q", snapshot.Persistence)
	}
	if !validWayland(snapshot.Wayland) {
		return fmt.Errorf("invalid wayland state %q", snapshot.Wayland)
	}

	seen := make(map[string]struct{}, len(snapshot.Entries))
	bytes := uint64(0)
	seenUnpinned := false
	for _, entry := range snapshot.Entries {
		if err := ValidateEntry(entry); err != nil {
			return err
		}
		if _, ok := seen[entry.ID]; ok {
			return fmt.Errorf("duplicate entry id %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		if entry.Pinned {
			if seenUnpinned {
				return fmt.Errorf("pinned entry follows an unpinned entry")
			}
		} else {
			seenUnpinned = true
		}
		if entry.Size > MaxTotalBytes-bytes {
			return fmt.Errorf("snapshot payload total exceeds %d bytes", MaxTotalBytes)
		}
		bytes += entry.Size
	}
	return nil
}

func ValidateDelta(delta Delta) error {
	if len(delta.Changes) == 0 || len(delta.Changes) > MaxChanges {
		return fmt.Errorf("delta has %d changes", len(delta.Changes))
	}
	if delta.Persistence != "" && !validPersistence(delta.Persistence) {
		return fmt.Errorf("invalid persistence state %q", delta.Persistence)
	}
	if delta.Wayland != "" && !validWayland(delta.Wayland) {
		return fmt.Errorf("invalid wayland state %q", delta.Wayland)
	}
	seen := make(map[string]struct{}, len(delta.Changes))
	for _, change := range delta.Changes {
		if err := ValidateChange(change); err != nil {
			return err
		}
		if _, ok := seen[change.ID]; ok {
			return fmt.Errorf("duplicate change id %q", change.ID)
		}
		seen[change.ID] = struct{}{}
	}
	return nil
}

func ValidateChange(change Change) error {
	if err := validateRequiredID(change.ID); err != nil {
		return err
	}
	switch change.Kind {
	case ChangeAdded, ChangeMoved, ChangeUpdated:
		if change.Entry == nil {
			return fmt.Errorf("%s change has no entry", change.Kind)
		}
		if change.Entry.ID != change.ID {
			return fmt.Errorf("change id %q does not match entry id %q", change.ID, change.Entry.ID)
		}
		return ValidateEntry(*change.Entry)
	case ChangeRemoved:
		if change.Entry != nil {
			return fmt.Errorf("removed change contains an entry")
		}
		return nil
	default:
		return fmt.Errorf("unknown change kind %q", change.Kind)
	}
}

func ValidateEntry(entry Entry) error {
	if err := validateRequiredID(entry.ID); err != nil {
		return err
	}
	if entry.Kind != KindText && entry.Kind != KindImage {
		return fmt.Errorf("invalid entry kind %q", entry.Kind)
	}
	if !validMIME(entry.MIME) {
		return fmt.Errorf("invalid entry MIME %q", entry.MIME)
	}
	if len(entry.OfferedMIME) == 0 || len(entry.OfferedMIME) > MaxOfferedMIME {
		return fmt.Errorf("invalid offered MIME count %d", len(entry.OfferedMIME))
	}
	offered := false
	for _, mime := range entry.OfferedMIME {
		if !validMIME(mime) {
			return fmt.Errorf("invalid offered MIME %q", mime)
		}
		if mime == entry.MIME {
			offered = true
		}
	}
	if !offered {
		return fmt.Errorf("capture MIME %q is not offered", entry.MIME)
	}
	if entry.Size == 0 {
		return fmt.Errorf("entry size is zero")
	}
	var maxBytes uint64 = MaxImageBytes
	if entry.Kind == KindText {
		maxBytes = MaxTextBytes
	}
	if entry.Size > maxBytes {
		return fmt.Errorf("entry size %d exceeds %d", entry.Size, maxBytes)
	}
	if len(entry.SHA256) != 64 {
		return fmt.Errorf("entry SHA-256 has length %d", len(entry.SHA256))
	}
	if decoded, err := hex.DecodeString(entry.SHA256); err != nil || len(decoded) != 32 {
		return fmt.Errorf("entry SHA-256 is not hexadecimal")
	}
	if entry.CapturedAt.IsZero() {
		return fmt.Errorf("entry capture time is zero")
	}
	if len([]byte(entry.Preview)) > MaxPreviewBytes || !utf8.ValidString(entry.Preview) {
		return fmt.Errorf("entry preview is invalid or too long")
	}
	if entry.Kind == KindImage && entry.Preview != "" {
		return fmt.Errorf("image entry has a text preview")
	}
	return nil
}

func ValidateThumbnail(thumbnail Thumbnail) error {
	if err := validateRequiredID(thumbnail.ID); err != nil {
		return err
	}
	if !validMIME(thumbnail.MIME) {
		return fmt.Errorf("invalid thumbnail MIME %q", thumbnail.MIME)
	}
	if thumbnail.Width == 0 || int(thumbnail.Width) > MaxThumbnailPixels || thumbnail.Height == 0 || int(thumbnail.Height) > MaxThumbnailPixels {
		return fmt.Errorf("invalid thumbnail dimensions %dx%d", thumbnail.Width, thumbnail.Height)
	}
	if len(thumbnail.Data) == 0 || len(thumbnail.Data) > MaxThumbnailBytes {
		return fmt.Errorf("invalid thumbnail data size %d", len(thumbnail.Data))
	}
	return nil
}

func validateHello(hello *Hello) error {
	if hello == nil {
		return nil
	}
	if len(hello.Capabilities) > MaxCapabilities {
		return fmt.Errorf("too many capabilities")
	}
	for _, capability := range hello.Capabilities {
		if len([]byte(capability)) == 0 || len([]byte(capability)) > MaxCapabilityBytes || !utf8.ValidString(capability) {
			return fmt.Errorf("invalid capability")
		}
	}
	if hello.Persistence != "" && !validPersistence(hello.Persistence) {
		return fmt.Errorf("invalid persistence state %q", hello.Persistence)
	}
	if hello.Wayland != "" && !validWayland(hello.Wayland) {
		return fmt.Errorf("invalid wayland state %q", hello.Wayland)
	}
	return nil
}

func validateAck(ack Ack) error {
	return validateOptionalID(ack.RequestID, "ack request_id")
}

func validateError(body ErrorBody) error {
	if !validErrorCode(body.Code) {
		return fmt.Errorf("invalid error code %q", body.Code)
	}
	if len([]byte(body.Message)) == 0 || len([]byte(body.Message)) > MaxPreviewBytes || !utf8.ValidString(body.Message) {
		return fmt.Errorf("invalid error message")
	}
	return validateOptionalID(body.RequestID, "error request_id")
}

func noCommandFields(message Message, allowID, allowPinned, allowScope, allowMaxPX bool) error {
	if !allowID && message.ID != "" {
		return fmt.Errorf("message contains unexpected id")
	}
	if !allowPinned && message.Pinned != nil {
		return fmt.Errorf("message contains unexpected pinned value")
	}
	if !allowScope && message.Scope != "" {
		return fmt.Errorf("message contains unexpected scope")
	}
	if !allowMaxPX && message.MaxPX != 0 {
		return fmt.Errorf("message contains unexpected max_px")
	}
	if message.Type != TypeHello && message.Hello != nil {
		return fmt.Errorf("message contains unexpected hello body")
	}
	if message.Type != TypeSnapshot && message.Snapshot != nil {
		return fmt.Errorf("message contains unexpected snapshot body")
	}
	if message.Type != TypeDelta && message.Delta != nil {
		return fmt.Errorf("message contains unexpected delta body")
	}
	if message.Type != TypeAck && message.Ack != nil {
		return fmt.Errorf("message contains unexpected ack body")
	}
	if message.Type != TypeError && message.Error != nil {
		return fmt.Errorf("message contains unexpected error body")
	}
	if message.Type != TypeThumbnail && message.Thumbnail != nil {
		return fmt.Errorf("message contains unexpected thumbnail body")
	}
	return nil
}

func validMessageType(messageType MessageType) bool {
	switch messageType {
	case TypeHello, TypeSnapshot, TypeDelta, TypeAck, TypeError, TypeRestore, TypePin, TypeDelete, TypeClear, TypeThumbnail, TypeResync:
		return true
	default:
		return false
	}
}

func validPersistence(state PersistenceState) bool {
	return state == PersistenceDurable || state == PersistenceVolatile || state == PersistenceUnavailable
}

func validWayland(state WaylandState) bool {
	return state == WaylandReady || state == WaylandUnavailable
}

func validErrorCode(code ErrorCode) bool {
	switch code {
	case ErrorUnsupported, ErrorNotFound, ErrorLimit, ErrorUnavailable, ErrorPersistence, ErrorProtocol, ErrorConflict:
		return true
	default:
		return false
	}
}

func validMIME(value string) bool {
	if len(value) == 0 || len([]byte(value)) > MaxMIMEBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func validateRequiredID(value string) error {
	if err := validateOptionalID(value, "id"); err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("id is empty")
	}
	return nil
}

func validateOptionalID(value, field string) error {
	if len([]byte(value)) > MaxIDBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%s is invalid or too long", field)
	}
	for _, r := range value {
		if r == '/' || r == '\\' || r < 0x21 || r > 0x7e {
			return fmt.Errorf("%s contains an unsafe character", field)
		}
	}
	return nil
}

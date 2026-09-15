// Package history owns the bounded, ordered clipboard records.
package history

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

var (
	ErrEmptyText       = errors.New("empty text clipboard capture")
	ErrInvalidCapture  = errors.New("invalid clipboard capture")
	ErrLimit           = errors.New("clipboard history limit exceeded")
	ErrStaleGeneration = errors.New("stale clipboard capture generation")
	ErrNotFound        = errors.New("clipboard history entry not found")
	ErrInvalidScope    = errors.New("invalid clipboard history clear scope")
)

const (
	ChangeAdded   = protocol.ChangeAdded
	ChangeMoved   = protocol.ChangeMoved
	ChangeUpdated = protocol.ChangeUpdated
	ChangeRemoved = protocol.ChangeRemoved
)

type Capture struct {
	Generation  uint64
	Kind        protocol.Kind
	MIME        string
	OfferedMIME []string
	Payload     []byte
	CapturedAt  time.Time
}

type Item struct {
	metadata protocol.Entry
	payload  []byte
}

func (item Item) Metadata() protocol.Entry {
	return cloneEntry(item.metadata)
}

func (item Item) Payload() []byte {
	return bytes.Clone(item.payload)
}

func NewItem(metadata protocol.Entry, payload []byte) (Item, error) {
	if err := protocol.ValidateEntry(metadata); err != nil {
		return Item{}, fmt.Errorf("%w: %v", ErrInvalidCapture, err)
	}
	if uint64(len(payload)) != metadata.Size {
		return Item{}, fmt.Errorf("%w: payload size does not match metadata", ErrInvalidCapture)
	}
	hash := sha256.Sum256(payload)
	if metadata.SHA256 != hex.EncodeToString(hash[:]) {
		return Item{}, fmt.Errorf("%w: payload hash does not match metadata", ErrInvalidCapture)
	}
	return Item{metadata: cloneEntry(metadata), payload: bytes.Clone(payload)}, nil
}

type record struct {
	metadata protocol.Entry
	payload  []byte
}

type History struct {
	generation uint64
	maxEntries int
	maxBytes   uint64
	entries    []record
	totalBytes uint64
}

func New(generation uint64) *History {
	return newWithLimits(generation, protocol.MaxEntries, protocol.MaxTotalBytes)
}

// newWithLimits keeps algorithm tests small while production uses the fixed
// protocol limits.
func newWithLimits(generation uint64, maxEntries int, maxBytes uint64) *History {
	return &History{generation: generation, maxEntries: maxEntries, maxBytes: maxBytes}
}

func (history *History) SetGeneration(generation uint64) {
	history.generation = generation
}

func (history *History) Generation() uint64 {
	return history.generation
}

func (history *History) Capture(capture Capture) ([]protocol.Change, error) {
	if capture.Generation != history.generation {
		return nil, ErrStaleGeneration
	}
	if capture.Kind != protocol.KindText && capture.Kind != protocol.KindImage {
		return nil, fmt.Errorf("%w: unsupported kind %q", ErrInvalidCapture, capture.Kind)
	}
	if capture.Kind == protocol.KindText && len(capture.Payload) == 0 {
		return nil, ErrEmptyText
	}
	if len(capture.Payload) == 0 {
		return nil, fmt.Errorf("%w: empty image payload", ErrInvalidCapture)
	}
	var maxBytes uint64 = protocol.MaxImageBytes
	if capture.Kind == protocol.KindText {
		maxBytes = protocol.MaxTextBytes
	}
	if uint64(len(capture.Payload)) > maxBytes {
		return nil, ErrLimit
	}
	offered := append([]string(nil), capture.OfferedMIME...)
	if len(offered) == 0 {
		offered = []string{capture.MIME}
	}
	if len(offered) > protocol.MaxOfferedMIME {
		return nil, fmt.Errorf("%w: too many offered MIME types", ErrInvalidCapture)
	}

	capturedAt := capture.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	} else {
		capturedAt = capturedAt.UTC()
	}
	payload := bytes.Clone(capture.Payload)
	hash := sha256.Sum256(payload)
	metadata := protocol.Entry{
		Kind:        capture.Kind,
		MIME:        capture.MIME,
		OfferedMIME: offered,
		Size:        uint64(len(payload)),
		SHA256:      hex.EncodeToString(hash[:]),
		CapturedAt:  capturedAt,
		Preview:     preview(payload, capture.Kind),
	}

	duplicate := -1
	for index, entry := range history.entries {
		if samePayload(entry.metadata, capture.Kind, capture.MIME, payload) {
			duplicate = index
			break
		}
	}

	if duplicate >= 0 {
		old := history.entries[duplicate]
		metadata.ID = old.metadata.ID
		metadata.Pinned = old.metadata.Pinned
		metadata.OfferedMIME = mergeMIME(old.metadata.OfferedMIME, metadata.OfferedMIME)
		if err := protocol.ValidateEntry(metadata); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidCapture, err)
		}

		next := append([]record(nil), history.entries...)
		next = append(next[:duplicate], next[duplicate+1:]...)
		updated := record{metadata: metadata, payload: payload}
		insertAt := pinnedCount(next)
		if updated.metadata.Pinned {
			insertAt = 0
		}
		next = insertRecord(next, insertAt, updated)
		history.entries = next
		return []protocol.Change{changeFor(protocol.ChangeMoved, updated)}, nil
	}

	var err error
	metadata.ID, err = newID()
	if err != nil {
		return nil, fmt.Errorf("generate clipboard entry ID: %w", err)
	}
	if err := protocol.ValidateEntry(metadata); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCapture, err)
	}
	candidate := record{metadata: metadata, payload: payload}
	next := append([]record(nil), history.entries...)
	nextTotal := history.totalBytes
	changes := make([]protocol.Change, 0)
	for len(next)+1 > history.maxEntries || nextTotal+metadata.Size > history.maxBytes {
		if len(next) == 0 || next[len(next)-1].metadata.Pinned {
			return nil, ErrLimit
		}
		removed := next[len(next)-1]
		next = next[:len(next)-1]
		nextTotal -= removed.metadata.Size
		changes = append(changes, changeFor(protocol.ChangeRemoved, removed))
	}
	insertAt := pinnedCount(next)
	next = insertRecord(next, insertAt, candidate)
	nextTotal += metadata.Size
	history.entries = next
	history.totalBytes = nextTotal
	changes = append(changes, changeFor(protocol.ChangeAdded, candidate))
	return changes, nil
}

func (history *History) SetPinned(id string, pinned bool) ([]protocol.Change, error) {
	index := history.indexOf(id)
	if index < 0 {
		return nil, ErrNotFound
	}
	if history.entries[index].metadata.Pinned == pinned {
		return nil, nil
	}
	next := append([]record(nil), history.entries...)
	entry := next[index]
	entry.metadata.Pinned = pinned
	next = append(next[:index], next[index+1:]...)
	insertAt := pinnedCount(next)
	if pinned {
		insertAt = 0
	}
	next = insertRecord(next, insertAt, entry)
	history.entries = next
	return []protocol.Change{changeFor(protocol.ChangeUpdated, entry)}, nil
}

func (history *History) Delete(id string) ([]protocol.Change, error) {
	index := history.indexOf(id)
	if index < 0 {
		return nil, ErrNotFound
	}
	removed := history.entries[index]
	history.entries = append(history.entries[:index], history.entries[index+1:]...)
	history.totalBytes -= removed.metadata.Size
	return []protocol.Change{changeFor(protocol.ChangeRemoved, removed)}, nil
}

func (history *History) Clear(scope protocol.ClearScope) ([]protocol.Change, error) {
	if scope != protocol.ClearUnpinned && scope != protocol.ClearAll {
		return nil, ErrInvalidScope
	}
	if len(history.entries) == 0 {
		return nil, nil
	}
	kept := make([]record, 0, len(history.entries))
	changes := make([]protocol.Change, 0, len(history.entries))
	var total uint64
	for _, entry := range history.entries {
		if scope == protocol.ClearUnpinned && entry.metadata.Pinned {
			kept = append(kept, entry)
			total += entry.metadata.Size
			continue
		}
		changes = append(changes, changeFor(protocol.ChangeRemoved, entry))
	}
	history.entries = kept
	history.totalBytes = total
	return changes, nil
}

func (history *History) Snapshot() []protocol.Entry {
	entries := make([]protocol.Entry, len(history.entries))
	for index, entry := range history.entries {
		entries[index] = cloneEntry(entry.metadata)
	}
	return entries
}

func (history *History) Items() []Item {
	items := make([]Item, len(history.entries))
	for index, entry := range history.entries {
		items[index] = Item{metadata: cloneEntry(entry.metadata), payload: bytes.Clone(entry.payload)}
	}
	return items
}

func (history *History) Replace(items []Item) error {
	if len(items) > history.maxEntries {
		return ErrLimit
	}
	next := make([]record, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	var total uint64
	seenUnpinned := false
	for _, item := range items {
		valid, err := NewItem(item.Metadata(), item.Payload())
		if err != nil {
			return err
		}
		metadata := valid.Metadata()
		if _, ok := seen[metadata.ID]; ok {
			return fmt.Errorf("%w: duplicate entry ID %q", ErrInvalidCapture, metadata.ID)
		}
		seen[metadata.ID] = struct{}{}
		if metadata.Pinned {
			if seenUnpinned {
				return fmt.Errorf("%w: pinned entry follows an unpinned entry", ErrInvalidCapture)
			}
		} else {
			seenUnpinned = true
		}
		if metadata.Size > history.maxBytes-total {
			return ErrLimit
		}
		total += metadata.Size
		next = append(next, record{metadata: metadata, payload: valid.Payload()})
	}
	history.entries = next
	history.totalBytes = total
	return nil
}

func (history *History) Count() int {
	return len(history.entries)
}

func (history *History) TotalBytes() uint64 {
	return history.totalBytes
}

func (history *History) Item(id string) (Item, bool) {
	index := history.indexOf(id)
	if index < 0 {
		return Item{}, false
	}
	entry := history.entries[index]
	return Item{metadata: cloneEntry(entry.metadata), payload: bytes.Clone(entry.payload)}, true
}

func (history *History) indexOf(id string) int {
	for index, entry := range history.entries {
		if entry.metadata.ID == id {
			return index
		}
	}
	return -1
}

func changeFor(kind protocol.ChangeKind, entry record) protocol.Change {
	metadata := cloneEntry(entry.metadata)
	if kind == protocol.ChangeRemoved {
		return protocol.Change{Kind: kind, ID: metadata.ID}
	}
	return protocol.Change{Kind: kind, ID: metadata.ID, Entry: &metadata}
}

func samePayload(entry protocol.Entry, kind protocol.Kind, mime string, payload []byte) bool {
	if entry.Kind != kind {
		return false
	}
	hash := sha256.Sum256(payload)
	if entry.SHA256 != hex.EncodeToString(hash[:]) {
		return false
	}
	if kind == protocol.KindImage {
		return entry.MIME == mime
	}
	return canonicalTextMIME(entry.MIME) == canonicalTextMIME(mime)
}

func canonicalTextMIME(mime string) string {
	lower := strings.ToLower(mime)
	switch lower {
	case "text/plain", "text/plain;charset=utf-8", "utf8_string":
		return "text/plain"
	default:
		return lower
	}
}

func mergeMIME(old, current []string) []string {
	merged := make([]string, 0, min(protocol.MaxOfferedMIME, len(old)+len(current)))
	for _, values := range [][]string{current, old} {
		for _, value := range values {
			seen := false
			for _, existing := range merged {
				if existing == value {
					seen = true
					break
				}
			}
			if !seen && len(merged) < protocol.MaxOfferedMIME {
				merged = append(merged, value)
			}
		}
	}
	return merged
}

func preview(payload []byte, kind protocol.Kind) string {
	if kind != protocol.KindText {
		return ""
	}
	value := string(payload)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "\uFFFD")
	}
	if len([]byte(value)) <= protocol.MaxPreviewBytes {
		return value
	}
	cut := protocol.MaxPreviewBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func newID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func cloneEntry(entry protocol.Entry) protocol.Entry {
	entry.OfferedMIME = append([]string(nil), entry.OfferedMIME...)
	return entry
}

func insertRecord(entries []record, index int, entry record) []record {
	entries = append(entries, record{})
	copy(entries[index+1:], entries[index:])
	entries[index] = entry
	return entries
}

func pinnedCount(entries []record) int {
	for index, entry := range entries {
		if !entry.metadata.Pinned {
			return index
		}
	}
	return len(entries)
}

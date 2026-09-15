package history

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

func capture(generation uint64, kind protocol.Kind, mime string, payload string, when time.Time) Capture {
	return Capture{
		Generation:  generation,
		Kind:        kind,
		MIME:        mime,
		OfferedMIME: []string{mime},
		Payload:     []byte(payload),
		CapturedAt:  when,
	}
}

func TestHistoryKeepsPinnedNewestFirstAndUnpinnedNewestFirst(t *testing.T) {
	h := New(9)
	first := capture(9, protocol.KindText, "text/plain", "first", time.Unix(1, 0).UTC())
	second := capture(9, protocol.KindText, "text/plain", "second", time.Unix(2, 0).UTC())
	third := capture(9, protocol.KindText, "text/plain", "third", time.Unix(3, 0).UTC())

	firstChanges, err := h.Capture(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstChanges) != 1 || firstChanges[0].Kind != ChangeAdded {
		t.Fatalf("first changes = %+v", firstChanges)
	}
	if _, err := h.Capture(second); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(third); err != nil {
		t.Fatal(err)
	}

	entries := h.Snapshot()
	if got := entries[0].Preview; got != "third" {
		t.Fatalf("newest preview = %q", got)
	}
	if _, err := h.SetPinned(entries[2].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetPinned(entries[1].ID, true); err != nil {
		t.Fatal(err)
	}
	entries = h.Snapshot()
	if got := []string{entries[0].Preview, entries[1].Preview, entries[2].Preview}; !equalStrings(got, []string{"second", "first", "third"}) {
		t.Fatalf("ordered previews = %v", got)
	}
}

func TestTextAliasesDeduplicateAndMoveExistingEntry(t *testing.T) {
	h := New(1)
	when := time.Unix(1, 0).UTC()
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", "same", when)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain;charset=utf-8", "other", when.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	changes, err := h.Capture(capture(1, protocol.KindText, "UTF8_STRING", "same", when.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != ChangeMoved {
		t.Fatalf("duplicate changes = %+v", changes)
	}
	entries := h.Snapshot()
	if len(entries) != 2 || entries[0].Preview != "same" || entries[0].MIME != "UTF8_STRING" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestImagesRequireExactMIMEAndBytes(t *testing.T) {
	h := New(1)
	when := time.Unix(1, 0).UTC()
	for _, c := range []Capture{
		capture(1, protocol.KindImage, "image/png", "png", when),
		capture(1, protocol.KindImage, "image/jpeg", "png", when.Add(time.Second)),
		capture(1, protocol.KindImage, "image/png", "PNG", when.Add(2*time.Second)),
	} {
		if _, err := h.Capture(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(h.Snapshot()); got != 3 {
		t.Fatalf("image entry count = %d, want 3", got)
	}
}

func TestCaptureMatchingPinnedEntryDoesNotCreateUnpinnedEcho(t *testing.T) {
	h := New(1)
	when := time.Unix(1, 0).UTC()
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", "same", when)); err != nil {
		t.Fatal(err)
	}
	id := h.Snapshot()[0].ID
	if _, err := h.SetPinned(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(capture(1, protocol.KindText, "UTF8_STRING", "same", when.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	entries := h.Snapshot()
	if len(entries) != 1 || !entries[0].Pinned || entries[0].ID != id {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestHistoryEvictsOnlyOldestUnpinnedAtEntryAndByteLimits(t *testing.T) {
	h := newWithLimits(1, 3, 13)
	when := time.Unix(1, 0).UTC()
	for i, payload := range []string{"one", "two", "three"} {
		if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", payload, when.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	entries := h.Snapshot()
	if got := previews(entries); !equalStrings(got, []string{"three", "two", "one"}) {
		t.Fatalf("before eviction = %v", got)
	}
	if _, err := h.SetPinned(entries[2].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", "four", when.Add(4*time.Second))); err != nil {
		t.Fatal(err)
	}
	if got := previews(h.Snapshot()); !equalStrings(got, []string{"one", "four", "three"}) {
		t.Fatalf("after eviction = %v", got)
	}

	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", "1234567890", when.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	if got := previews(h.Snapshot()); !equalStrings(got, []string{"one", "1234567890"}) {
		t.Fatalf("after byte eviction = %v", got)
	}
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", "abcdefghijk", when.Add(6*time.Second))); !errors.Is(err, ErrLimit) {
		t.Fatalf("oversized insertion error = %v, want ErrLimit", err)
	}
}

func TestHistoryRejectsPerEntryLimitsEmptyTextAndStaleGeneration(t *testing.T) {
	h := New(4)
	when := time.Unix(1, 0).UTC()
	cases := []struct {
		name    string
		capture Capture
		err     error
	}{
		{name: "empty text", capture: capture(4, protocol.KindText, "text/plain", "", when), err: ErrEmptyText},
		{name: "text limit", capture: Capture{Generation: 4, Kind: protocol.KindText, MIME: "text/plain", OfferedMIME: []string{"text/plain"}, Payload: bytes.Repeat([]byte{'x'}, protocol.MaxTextBytes+1), CapturedAt: when}, err: ErrLimit},
		{name: "image limit", capture: Capture{Generation: 4, Kind: protocol.KindImage, MIME: "image/png", OfferedMIME: []string{"image/png"}, Payload: bytes.Repeat([]byte{'x'}, protocol.MaxImageBytes+1), CapturedAt: when}, err: ErrLimit},
		{name: "stale generation", capture: capture(3, protocol.KindText, "text/plain", "stale", when), err: ErrStaleGeneration},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.Capture(tc.capture); !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if got := len(h.Snapshot()); got != 0 {
				t.Fatalf("failed capture changed history: %d entries", got)
			}
		})
	}
}

func TestTextPreviewIsUTF8SafeAndBounded(t *testing.T) {
	h := New(1)
	payload := strings.Repeat("界", protocol.MaxPreviewBytes)
	if _, err := h.Capture(capture(1, protocol.KindText, "text/plain", payload, time.Unix(1, 0).UTC())); err != nil {
		t.Fatal(err)
	}
	preview := h.Snapshot()[0].Preview
	if len([]byte(preview)) > protocol.MaxPreviewBytes || !utf8Valid(preview) {
		t.Fatalf("preview length/encoding invalid: bytes=%d", len([]byte(preview)))
	}
}

func TestHistorySnapshotsDoNotExposeMutableState(t *testing.T) {
	h := New(1)
	if _, err := h.Capture(Capture{Generation: 1, Kind: protocol.KindText, MIME: "text/plain", OfferedMIME: []string{"text/plain"}, Payload: []byte("hello"), CapturedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	entries := h.Snapshot()
	entries[0].OfferedMIME[0] = "changed"
	items := h.Items()
	payload := items[0].Payload()
	payload[0] = 'X'
	if h.Snapshot()[0].OfferedMIME[0] != "text/plain" || string(h.Items()[0].Payload()) != "hello" {
		t.Fatal("snapshot exposed mutable history state")
	}
}

func previews(entries []protocol.Entry) []string {
	result := make([]string, len(entries))
	for i, entry := range entries {
		result[i] = entry.Preview
	}
	return result
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func utf8Valid(value string) bool {
	for len(value) > 0 {
		_, size := rangeFirstRune(value)
		if size == 0 {
			return false
		}
		value = value[size:]
	}
	return true
}

func rangeFirstRune(value string) (rune, int) {
	for _, r := range value {
		return r, len(string(r))
	}
	return 0, 0
}

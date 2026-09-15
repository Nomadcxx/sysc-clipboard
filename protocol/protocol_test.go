package protocol

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func validEntry() Entry {
	return Entry{
		ID:          "entry-1",
		Kind:        KindText,
		MIME:        "text/plain",
		OfferedMIME: []string{"text/plain", "UTF8_STRING"},
		Size:        5,
		SHA256:      strings.Repeat("a", 64),
		CapturedAt:  time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		Preview:     "hello",
	}
}

func TestValidateMessageAcceptsValidMessages(t *testing.T) {
	for _, msg := range []Message{
		{Version: Version, Type: TypeHello},
		{Version: Version, Type: TypeRestore, ID: "entry-1"},
		{Version: Version, Type: TypePin, ID: "entry-1", Pinned: boolPtr(true)},
		{Version: Version, Type: TypeClear, Scope: ClearUnpinned},
		{Version: Version, Type: TypeSnapshot, Snapshot: &Snapshot{
			Revision:    4,
			Entries:     []Entry{validEntry()},
			Persistence: PersistenceDurable,
			Wayland:     WaylandReady,
		}},
		{Version: Version, Type: TypeDelta, Delta: &Delta{
			Revision: 5,
			Changes: []Change{{Kind: ChangeAdded, ID: validEntry().ID, Entry: &Entry{ // duplicate copy keeps this test independent of pointer identity
				ID:          validEntry().ID,
				Kind:        validEntry().Kind,
				MIME:        validEntry().MIME,
				OfferedMIME: validEntry().OfferedMIME,
				Size:        validEntry().Size,
				SHA256:      validEntry().SHA256,
				CapturedAt:  validEntry().CapturedAt,
				Preview:     validEntry().Preview,
			}}},
		}},
	} {
		if err := ValidateMessage(msg); err != nil {
			t.Errorf("ValidateMessage(%s) = %v", msg.Type, err)
		}
	}
}

func TestValidateMessageRejectsInvalidVersionsAndTypes(t *testing.T) {
	for _, msg := range []Message{
		{Version: Version + 1, Type: TypeHello},
		{Version: Version, Type: MessageType("unknown")},
		{Version: Version, Type: TypeRestore},
		{Version: Version, Type: TypePin, ID: "entry-1"},
		{Version: Version, Type: TypeClear, Scope: ClearScope("everything")},
	} {
		if err := ValidateMessage(msg); err == nil {
			t.Errorf("ValidateMessage(%+v) succeeded", msg)
		}
	}
}

func TestValidateMessageRejectsOverBoundEntryMetadata(t *testing.T) {
	cases := []struct {
		name  string
		entry Entry
	}{
		{name: "id", entry: func() Entry {
			e := validEntry()
			e.ID = strings.Repeat("x", MaxIDBytes+1)
			return e
		}()},
		{name: "mime", entry: func() Entry {
			e := validEntry()
			e.MIME = strings.Repeat("x", MaxMIMEBytes+1)
			return e
		}()},
		{name: "offered mime count", entry: func() Entry {
			e := validEntry()
			e.OfferedMIME = make([]string, MaxOfferedMIME+1)
			for i := range e.OfferedMIME {
				e.OfferedMIME[i] = "text/plain"
			}
			return e
		}()},
		{name: "preview", entry: func() Entry {
			e := validEntry()
			e.Preview = strings.Repeat("x", MaxPreviewBytes+1)
			return e
		}()},
		{name: "size", entry: func() Entry {
			e := validEntry()
			e.Size = MaxTextBytes + 1
			return e
		}()},
		{name: "hash", entry: func() Entry {
			e := validEntry()
			e.SHA256 = "not-a-sha256"
			return e
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := Message{Version: Version, Type: TypeSnapshot, Snapshot: &Snapshot{
				Revision:    1,
				Entries:     []Entry{tc.entry},
				Persistence: PersistenceVolatile,
				Wayland:     WaylandUnavailable,
			}}
			if err := ValidateMessage(msg); err == nil {
				t.Fatalf("ValidateMessage succeeded for invalid %s", tc.name)
			}
		})
	}
}

func TestValidateMessageRejectsSnapshotBoundsAndInvalidTextPreview(t *testing.T) {
	entry := validEntry()
	entry.Preview = string([]byte{0xff})
	if err := ValidateEntry(entry); err == nil {
		t.Fatal("invalid UTF-8 preview accepted")
	}

	entries := make([]Entry, MaxEntries+1)
	for i := range entries {
		entries[i] = entry
		entries[i].ID = "entry-" + string(rune('a'+i%26))
	}
	if err := ValidateSnapshot(Snapshot{Revision: 1, Entries: entries, Persistence: PersistenceDurable, Wayland: WaylandReady}); err == nil {
		t.Fatal("oversized snapshot accepted")
	}

	tooLarge := validEntry()
	tooLarge.Size = MaxTotalBytes
	if err := ValidateSnapshot(Snapshot{Revision: 1, Entries: []Entry{tooLarge}, Persistence: PersistenceDurable, Wayland: WaylandReady}); err == nil {
		t.Fatal("oversized snapshot byte total accepted")
	}
}

func TestWriteAndReadMessageRoundTrip(t *testing.T) {
	want := Message{Version: Version, Type: TypeSnapshot, Snapshot: &Snapshot{
		Revision:    7,
		Entries:     []Entry{validEntry()},
		Persistence: PersistenceDurable,
		Wayland:     WaylandReady,
	}}
	var wire bytes.Buffer
	if err := WriteMessage(&wire, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.Snapshot == nil || got.Snapshot.Revision != want.Snapshot.Revision {
		t.Fatalf("message = %+v, want %+v", got, want)
	}
}

func boolPtr(v bool) *bool { return &v }

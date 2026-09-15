package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

func TestSealOpenRoundTripAndRejectsWrongKeyOrEntryID(t *testing.T) {
	key := bytes.Repeat([]byte{3}, KeyBytes)
	sealed, err := seal(key, payloadPurpose, "entry-1", []byte("clipboard bytes"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(key, payloadPurpose, "entry-1", sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "clipboard bytes" {
		t.Fatalf("opened payload = %q", got)
	}
	wrongKey := bytes.Repeat([]byte{4}, KeyBytes)
	if _, err := open(wrongKey, payloadPurpose, "entry-1", sealed); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong key error = %v, want ErrAuthentication", err)
	}
	if _, err := open(key, payloadPurpose, "entry-2", sealed); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong entry ID error = %v, want ErrAuthentication", err)
	}
}

func TestStoreRoundTripKeepsPrivateEncryptedFiles(t *testing.T) {
	key := bytes.Repeat([]byte{8}, KeyBytes)
	root := filepath.Join(t.TempDir(), "state")
	store, err := New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	items := testItems(t)
	if err := store.Save(items); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != len(items) || string(loaded.Items[0].Payload()) != "second" {
		t.Fatalf("loaded items = %d, payload = %q", len(loaded.Items), loaded.Items[0].Payload())
	}
	for _, tc := range []struct {
		name string
		path string
		perm os.FileMode
	}{
		{name: "state", path: root, perm: 0700},
		{name: "entries", path: store.entriesDir, perm: 0700},
		{name: "manifest", path: store.manifestPath, perm: 0600},
	} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != tc.perm {
			t.Fatalf("%s permissions = %o, want %o", tc.name, info.Mode().Perm(), tc.perm)
		}
	}
	entries, err := os.ReadDir(store.entriesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(items) {
		t.Fatalf("payload file count = %d, want %d", len(entries), len(items))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("payload %s permissions = %o", entry.Name(), info.Mode().Perm())
		}
		data, err := os.ReadFile(filepath.Join(store.entriesDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("second")) {
			t.Fatal("payload contains plaintext")
		}
	}
}

func TestLoadPreservesCorruptManifest(t *testing.T) {
	key := bytes.Repeat([]byte{9}, KeyBytes)
	store, err := New(filepath.Join(t.TempDir(), "state"), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testItems(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.manifestPath, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := store.Load()
	if !errors.Is(err, ErrCorruptManifest) || !result.ManifestCorrupt {
		t.Fatalf("corrupt manifest result = %+v, error = %v", result, err)
	}
	if _, err := os.Stat(store.manifestPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("original manifest still exists: %v", err)
	}
	corrupt, err := filepath.Glob(store.manifestPath + ".*.corrupt")
	if err != nil || len(corrupt) != 1 {
		t.Fatalf("preserved corrupt manifests = %v, %v", corrupt, err)
	}
}

func TestLoadDropsOnlyCorruptPayload(t *testing.T) {
	key := bytes.Repeat([]byte{10}, KeyBytes)
	store, err := New(filepath.Join(t.TempDir(), "state"), key)
	if err != nil {
		t.Fatal(err)
	}
	items := testItems(t)
	if err := store.Save(items); err != nil {
		t.Fatal(err)
	}
	corruptID := items[0].Metadata().ID
	path := filepath.Join(store.entriesDir, corruptID+".enc")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 1
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || len(result.Dropped) != 1 || result.Dropped[0] != corruptID {
		t.Fatalf("load result = %+v", result)
	}
	if got := string(result.Items[0].Payload()); got != "first" {
		t.Fatalf("surviving payload = %q", got)
	}
}

func TestSaveFailureLeavesPreviousManifestUsable(t *testing.T) {
	key := bytes.Repeat([]byte{11}, KeyBytes)
	store, err := New(filepath.Join(t.TempDir(), "state"), key)
	if err != nil {
		t.Fatal(err)
	}
	initial := testItems(t)[1:]
	if err := store.Save(initial); err != nil {
		t.Fatal(err)
	}
	oldManifest, err := os.ReadFile(store.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	store.writeFile = func(path string, data []byte, perm fs.FileMode) error {
		if path == store.manifestPath+".new" {
			return fmt.Errorf("injected manifest stage failure")
		}
		return os.WriteFile(path, data, perm)
	}
	if err := store.Save(testItems(t)); err == nil {
		t.Fatal("staged manifest failure was not returned")
	}
	newManifest, err := os.ReadFile(store.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newManifest, oldManifest) {
		t.Fatal("failed save replaced the previous manifest")
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 1 || string(loaded.Items[0].Payload()) != "first" {
		t.Fatalf("loaded after rollback = %+v", loaded)
	}
}

func testItems(t *testing.T) []history.Item {
	t.Helper()
	h := history.New(1)
	when := time.Unix(1, 0).UTC()
	if _, err := h.Capture(history.Capture{Generation: 1, Kind: protocol.KindText, MIME: "text/plain", OfferedMIME: []string{"text/plain"}, Payload: []byte("first"), CapturedAt: when}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Capture(history.Capture{Generation: 1, Kind: protocol.KindText, MIME: "text/plain", OfferedMIME: []string{"text/plain"}, Payload: []byte("second"), CapturedAt: when.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	return h.Items()
}

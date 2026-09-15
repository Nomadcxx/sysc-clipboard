package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Nomadcxx/sysc-clipboard/internal/history"
	"github.com/Nomadcxx/sysc-clipboard/internal/store"
	"github.com/Nomadcxx/sysc-clipboard/protocol"
)

func TestResolveStateDirRequiresAbsoluteOverride(t *testing.T) {
	if _, err := resolveStateDir("relative"); err == nil {
		t.Fatal("relative state directory accepted")
	}
	root := filepath.Join(t.TempDir(), "state")
	got, err := resolveStateDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("state directory = %q, want %q", got, root)
	}
}

func TestLoadStateReloadsEncryptedHistoryFromKeyFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	keyDir := t.TempDir()
	if err := os.Chmod(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDir, "clipboard.key")
	key := bytes.Repeat([]byte{7}, store.KeyBytes)
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	stateStore, err := store.New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	original := history.New(1)
	if _, err := original.Capture(history.Capture{
		Generation: 1,
		Kind:       protocol.KindText,
		MIME:       "text/plain",
		Payload:    []byte("persisted clipboard"),
		CapturedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Save(original.Items()); err != nil {
		t.Fatal(err)
	}

	state, err := loadState(root, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.persistence != protocol.PersistenceDurable || state.store == nil {
		t.Fatalf("loaded state = %+v", state)
	}
	items := state.history.Items()
	if len(items) != 1 || string(items[0].Payload()) != "persisted clipboard" {
		t.Fatalf("loaded history = %+v", items)
	}
}

func TestLoadStateFallsBackToVolatileAfterCorruptManifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	keyDir := t.TempDir()
	if err := os.Chmod(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDir, "clipboard.key")
	key := bytes.Repeat([]byte{8}, store.KeyBytes)
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	stateStore, err := store.New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateStore.Save(nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.enc"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}

	state, err := loadState(root, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.persistence != protocol.PersistenceVolatile || len(state.warnings) == 0 {
		t.Fatalf("corrupt state = %+v", state)
	}
}

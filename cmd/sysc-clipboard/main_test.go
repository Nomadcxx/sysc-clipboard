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

func TestParseOptionsDisablesCapture(t *testing.T) {
	parsed, err := parseOptions([]string{"--disable-capture"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.disableCapture {
		t.Fatal("disable-capture = false, want true")
	}
}

func TestParseOptionsAcceptsHistoryCaps(t *testing.T) {
	parsed, err := parseOptions([]string{
		"--max-entries=40",
		"--max-total-bytes=67108864",
		"--max-text-bytes=1048576",
		"--max-image-bytes=8388608",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := history.Limits{MaxEntries: 40, MaxTotalBytes: 67108864, MaxTextBytes: 1048576, MaxImageBytes: 8388608}
	if parsed.limits != want {
		t.Fatalf("limits = %+v, want %+v", parsed.limits, want)
	}
}

func TestParseOptionsDefaultsToProtocolHistoryCaps(t *testing.T) {
	parsed, err := parseOptions(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.limits != history.DefaultLimits() {
		t.Fatalf("limits = %+v, want defaults %+v", parsed.limits, history.DefaultLimits())
	}
}

func TestParseOptionsRejectsHistoryCapsOutsideProtocolLimits(t *testing.T) {
	for _, argument := range []string{
		"--max-entries=0",
		"--max-entries=101",
		"--max-total-bytes=0",
		"--max-total-bytes=268435457",
		"--max-text-bytes=0",
		"--max-text-bytes=4194305",
		"--max-image-bytes=0",
		"--max-image-bytes=33554433",
	} {
		t.Run(argument, func(t *testing.T) {
			if _, err := parseOptions([]string{argument}, &bytes.Buffer{}); err == nil {
				t.Fatalf("parseOptions(%q) succeeded, want an out-of-range error", argument)
			}
		})
	}
}

func TestLoadStateAppliesCapsToEncryptedHistoryFromKeyFile(t *testing.T) {
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
		Payload:    []byte("older"),
		CapturedAt: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
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

	limits := history.DefaultLimits()
	limits.MaxEntries = 1
	limits.MaxTotalBytes = uint64(len("persisted clipboard"))
	state, err := loadState(root, keyPath, limits)
	if err != nil {
		t.Fatal(err)
	}
	if state.persistence != protocol.PersistenceDurable || state.store == nil {
		t.Fatalf("loaded state = %+v", state)
	}
	items := state.history.Items()
	if len(items) != 1 || string(items[0].Payload()) != "persisted clipboard" || state.limits != limits {
		t.Fatalf("loaded history = %+v", items)
	}
}

func TestCheckStateReportsConfiguredHistoryCaps(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	limits := history.Limits{MaxEntries: 40, MaxTotalBytes: 64 << 20, MaxTextBytes: 1 << 20, MaxImageBytes: 8 << 20}
	state := runtimeState{
		root:        filepath.Join(t.TempDir(), "state"),
		history:     history.NewWithLimits(0, limits),
		persistence: protocol.PersistenceUnavailable,
		limits:      limits,
	}
	var output bytes.Buffer
	if err := checkState(state, false, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"max-entries: 40", "max-total-bytes: 67108864", "max-text-bytes: 1048576", "max-image-bytes: 8388608"} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Fatalf("check output %q is missing %q", output.String(), expected)
		}
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

	state, err := loadState(root, keyPath, history.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if state.persistence != protocol.PersistenceVolatile || len(state.warnings) == 0 {
		t.Fatalf("corrupt state = %+v", state)
	}
}

package main

import (
	"bytes"
	"context"
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

func TestParseOptionsAcceptsHistoryRetention(t *testing.T) {
	parsed, err := parseOptions([]string{"--auto-clear-days=30", "--clear-at-startup"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.autoClearDays != 30 || !parsed.clearAtStartup {
		t.Fatalf("retention options = %+v", parsed)
	}
}

func TestParseOptionsRejectsInvalidAutoClearDays(t *testing.T) {
	for _, argument := range []string{"--auto-clear-days=-1", "--auto-clear-days=106752"} {
		t.Run(argument, func(t *testing.T) {
			if _, err := parseOptions([]string{argument}, &bytes.Buffer{}); err == nil {
				t.Fatalf("parseOptions(%q) succeeded, want an out-of-range error", argument)
			}
		})
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
	if parsed.autoClearDays != 0 || parsed.clearAtStartup {
		t.Fatalf("default retention = days %d, clear-at-startup %t; want disabled", parsed.autoClearDays, parsed.clearAtStartup)
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
	if err := checkState(state, options{autoClearDays: 30, clearAtStartup: true}, &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"max-entries: 40", "max-total-bytes: 67108864", "max-text-bytes: 1048576", "max-image-bytes: 8388608",
		"auto-clear-days: 30", "clear-at-startup: enabled",
	} {
		if !bytes.Contains(output.Bytes(), []byte(expected)) {
			t.Fatalf("check output %q is missing %q", output.String(), expected)
		}
	}
}

func TestApplyStartupCleanupExpiresUnpinnedHistoryAndPersists(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	state, root, _ := retentionTestState(t, now)
	if err := applyStartupCleanup(&state, 1, false, now); err != nil {
		t.Fatal(err)
	}
	items := state.history.Items()
	if len(items) != 2 {
		t.Fatalf("history after TTL cleanup = %d items, want pinned and fresh items", len(items))
	}
	hasPinned, hasFresh, hasOld := false, false, false
	for _, entry := range state.history.Snapshot() {
		switch entry.Preview {
		case "pinned":
			hasPinned = entry.Pinned
		case "fresh":
			hasFresh = true
		case "old":
			hasOld = true
		}
	}
	if !hasPinned || !hasFresh || hasOld {
		t.Fatalf("history after TTL cleanup = %+v, want pinned/fresh only", state.history.Snapshot())
	}
	loaded, err := state.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 2 || state.persistence != protocol.PersistenceDurable {
		t.Fatalf("persisted cleanup = %d items, persistence %q", len(loaded.Items), state.persistence)
	}
	files, err := os.ReadDir(filepath.Join(root, "entries"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("TTL cleanup left %d encrypted payload files, want 2", len(files))
	}
}

func TestApplyStartupCleanupClearsAllHistoryAndPersists(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	state, root, _ := retentionTestState(t, now)
	if err := applyStartupCleanup(&state, 0, true, now); err != nil {
		t.Fatal(err)
	}
	if state.history.Count() != 0 {
		t.Fatalf("history after startup clear = %d items, want 0", state.history.Count())
	}
	loaded, err := state.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 0 {
		t.Fatalf("persisted startup clear left %d items", len(loaded.Items))
	}
	files, err := os.ReadDir(filepath.Join(root, "entries"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("startup clear left %d encrypted payload files", len(files))
	}
}

func TestApplyStartupClearRequiresPersistentStore(t *testing.T) {
	h := history.New(1)
	if _, err := h.Capture(history.Capture{
		Generation: 1, Kind: protocol.KindText, MIME: "text/plain", Payload: []byte("keep until clear can persist"), CapturedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	state := runtimeState{history: h}
	if err := applyStartupCleanup(&state, 0, true, time.Now()); err == nil {
		t.Fatal("startup clear succeeded without a persistent store")
	}
	if got := h.Count(); got != 1 {
		t.Fatalf("history count after rejected startup clear = %d, want 1", got)
	}
}

func TestRunCheckReportsRetentionWithoutClearingHistory(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	state, root, keyPath := retentionTestState(t, now)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var output, errorsOutput bytes.Buffer
	code := run(context.Background(), []string{
		"--state-dir=" + root, "--key-file=" + keyPath, "--check", "--auto-clear-days=1", "--clear-at-startup",
	}, &output, &errorsOutput)
	if code != 0 {
		t.Fatalf("check exit = %d, stderr: %s", code, errorsOutput.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("auto-clear-days: 1")) || !bytes.Contains(output.Bytes(), []byte("clear-at-startup: enabled")) {
		t.Fatalf("check output = %q, missing retention settings", output.String())
	}
	loaded, err := state.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 3 {
		t.Fatalf("--check changed stored history: found %d entries, want 3", len(loaded.Items))
	}
}

func retentionTestState(t *testing.T, now time.Time) (runtimeState, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	keyDir := t.TempDir()
	if err := os.Chmod(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDir, "clipboard.key")
	key := bytes.Repeat([]byte{9}, store.KeyBytes)
	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		t.Fatal(err)
	}
	disk, err := store.New(root, key)
	if err != nil {
		t.Fatal(err)
	}
	h := history.New(1)
	for _, entry := range []struct {
		text       string
		capturedAt time.Time
	}{
		{text: "old", capturedAt: now.Add(-48 * time.Hour)},
		{text: "pinned", capturedAt: now.Add(-48 * time.Hour)},
		{text: "fresh", capturedAt: now.Add(-time.Hour)},
	} {
		if _, err := h.Capture(history.Capture{
			Generation: 1, Kind: protocol.KindText, MIME: "text/plain", Payload: []byte(entry.text), CapturedAt: entry.capturedAt,
		}); err != nil {
			t.Fatal(err)
		}
		if entry.text == "pinned" {
			if _, err := h.SetPinned(h.Snapshot()[0].ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := disk.Save(h.Items()); err != nil {
		t.Fatal(err)
	}
	state, err := loadState(root, keyPath, history.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return state, root, keyPath
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

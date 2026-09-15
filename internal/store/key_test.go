package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestKeyFileRequiresPrivateRegularExactKey(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	valid := bytes.Repeat([]byte{7}, KeyBytes)

	path := filepath.Join(root, "valid.key")
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, valid) {
		t.Fatal("valid key changed")
	}

	cases := []struct {
		name  string
		setup func(string) error
	}{
		{name: "wrong permissions", setup: func(path string) error {
			if err := os.WriteFile(path, valid, 0600); err != nil {
				return err
			}
			return os.Chmod(path, 0640)
		}},
		{name: "wrong length", setup: func(path string) error {
			return os.WriteFile(path, []byte("short"), 0600)
		}},
		{name: "non regular", setup: func(path string) error {
			return os.Mkdir(path, 0700)
		}},
		{name: "symlink", setup: func(path string) error {
			return os.Symlink(filepath.Join(root, "valid.key"), path)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := tc.setup(path); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadKeyFile(path); err == nil {
				t.Fatal("invalid key file accepted")
			}
		})
	}
}

func TestLoadOrCreateKeyFileCreatesPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "clipboard.key")
	key, err := LoadOrCreateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeyBytes {
		t.Fatalf("key length = %d", len(key))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions = %o", info.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0700 {
		t.Fatalf("parent permissions = %o", parent.Mode().Perm())
	}
	loaded, err := LoadOrCreateKeyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, key) {
		t.Fatal("existing key was replaced")
	}
}

func TestResolveStateDirUsesXDGThenHome(t *testing.T) {
	got, err := resolveStateDir("/state", "/home/example")
	if err != nil || got != filepath.Join("/state", "sysc-clipboard") {
		t.Fatalf("XDG state dir = %q, %v", got, err)
	}
	got, err = resolveStateDir("", "/home/example")
	if err != nil || got != filepath.Join("/home/example", ".local", "state", "sysc-clipboard") {
		t.Fatalf("home state dir = %q, %v", got, err)
	}
	if _, err := resolveStateDir("relative", "/home/example"); err == nil {
		t.Fatal("relative XDG state dir accepted")
	}
	if _, err := resolveStateDir("", ""); !errors.Is(err, ErrStateDirectory) {
		t.Fatalf("missing home error = %v", err)
	}
}

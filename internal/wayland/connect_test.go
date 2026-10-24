package wayland

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// listenWaylandSocket starts a stub compositor socket at path and returns it
// with the listener closed once the test ends.
func listenWaylandSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close() })
}

// A compositor that exports an absolute WAYLAND_DISPLAY must be dialled as
// given. Joining it to XDG_RUNTIME_DIR produced
// /run/user/1000//run/user/1000/wayland-0 and the daemon never reached the
// compositor.
func TestRealBackendDialsAbsoluteWaylandDisplay(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "wayland-0")
	listenWaylandSocket(t, socketPath)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("WAYLAND_DISPLAY", socketPath)

	backend, err := newRealBackend()
	if err != nil {
		t.Fatalf("newRealBackend with an absolute WAYLAND_DISPLAY: %v", err)
	}
	if err := backend.context.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A relative WAYLAND_DISPLAY is joined to XDG_RUNTIME_DIR.
func TestRealBackendJoinsRelativeWaylandDisplayToRuntimeDir(t *testing.T) {
	runtimeDir := t.TempDir()
	listenWaylandSocket(t, filepath.Join(runtimeDir, "wayland-0"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	backend, err := newRealBackend()
	if err != nil {
		t.Fatalf("newRealBackend with a relative WAYLAND_DISPLAY: %v", err)
	}
	if err := backend.context.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// An absolute WAYLAND_DISPLAY must not need XDG_RUNTIME_DIR at all.
func TestRealBackendAbsoluteWaylandDisplayNeedsNoRuntimeDir(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "wayland-0")
	listenWaylandSocket(t, socketPath)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("WAYLAND_DISPLAY", socketPath)

	backend, err := newRealBackend()
	if err != nil {
		t.Fatalf("newRealBackend without XDG_RUNTIME_DIR: %v", err)
	}
	if err := backend.context.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// A missing WAYLAND_DISPLAY falls back to wayland-0 in XDG_RUNTIME_DIR.
func TestRealBackendDefaultsToWaylandZero(t *testing.T) {
	runtimeDir := t.TempDir()
	listenWaylandSocket(t, filepath.Join(runtimeDir, "wayland-0"))
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	os.Unsetenv("WAYLAND_DISPLAY")

	backend, err := newRealBackend()
	if err != nil {
		t.Fatalf("newRealBackend without WAYLAND_DISPLAY: %v", err)
	}
	if err := backend.context.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

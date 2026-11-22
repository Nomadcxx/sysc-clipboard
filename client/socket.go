package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	minReconnectDelay = 50 * time.Millisecond
	maxReconnectDelay = 2 * time.Second
	clientTimeout     = 5 * time.Second
)

// ErrUntrustedDaemon reports a daemon socket that another user serves.
var ErrUntrustedDaemon = errors.New("clipboard daemon socket is not served by the current user")

type dialFunc func(context.Context, string) (net.Conn, error)
type waitFunc func(context.Context, time.Duration) error

func watchConnectionContext(ctx context.Context, connection net.Conn) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	connection, err := (&net.Dialer{Timeout: clientTimeout}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := verifyDaemonPeer(connection, uint32(os.Getuid())); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

// verifyDaemonPeer checks the kernel's record of the process serving the
// socket, as the daemon checks its clients. It refuses a socket another user
// planted; a process of the same user can still serve one.
func verifyDaemonPeer(connection net.Conn, uid uint32) error {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("%w: not a Unix socket", ErrUntrustedDaemon)
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var credentialsErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, credentialsErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if credentialsErr != nil {
		return fmt.Errorf("read clipboard daemon credentials: %w", credentialsErr)
	}
	if credentials.Uid != uid {
		return fmt.Errorf("%w: uid %d", ErrUntrustedDaemon, credentials.Uid)
	}
	return nil
}

func waitReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nextReconnectDelay(delay time.Duration) time.Duration {
	if delay >= maxReconnectDelay/2 {
		return maxReconnectDelay
	}
	return delay * 2
}

// DefaultSocketPath resolves the per-user daemon socket.
func DefaultSocketPath() (string, error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" || !filepath.IsAbs(runtimeDir) {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is not an absolute path")
	}
	return filepath.Join(runtimeDir, "sysc-clipboard", "control.v1.sock"), nil
}

package client

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	minReconnectDelay = 50 * time.Millisecond
	maxReconnectDelay = 2 * time.Second
	clientTimeout     = 5 * time.Second
)

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
	return (&net.Dialer{Timeout: clientTimeout}).DialContext(ctx, "unix", path)
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

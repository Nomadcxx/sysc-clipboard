package wayland

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestReadFDRejectsPayloadPastLimit(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = readFD(context.Background(), int(reader.Fd()), 4)
	if !errors.Is(err, errFDLimit) {
		t.Fatalf("readFD error = %v, want %v", err, errFDLimit)
	}
}

func TestReadFDRequiresEOF(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	defer reader.Close()
	if _, err := writer.Write([]byte("complete")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = readFD(ctx, int(reader.Fd()), 32)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readFD error = %v, want deadline while writer stays open", err)
	}
}

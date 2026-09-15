package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type oneByteReader struct {
	r io.Reader
}

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}

func TestReadFrameHandlesSplitReads(t *testing.T) {
	want := []byte(`{"version":1,"type":"hello"}`)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, want); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFrame(oneByteReader{r: &wire})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestFrameRejectsZeroAndOversizedPayloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{name: "zero", wire: []byte{0, 0, 0, 0}},
		{name: "oversized", wire: []byte{0, 0x10, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tc.wire))
			if !errors.Is(err, ErrFrameSize) {
				t.Fatalf("error = %v, want ErrFrameSize", err)
			}
		})
	}
}

func TestReadMessageRejectsMalformedJSON(t *testing.T) {
	for _, payload := range []string{
		`{"version":1,"type":"hello"`,
		`{"version":1,"type":"hello"}{"version":1,"type":"hello"}`,
		strings.Repeat("[", MaxJSONDepth+1) + strings.Repeat("]", MaxJSONDepth+1),
	} {
		var wire bytes.Buffer
		if err := WriteFrame(&wire, []byte(payload)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMessage(&wire); err == nil {
			t.Fatalf("ReadMessage(%q) succeeded", payload[:min(len(payload), 32)])
		}
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteFrame(&wire, bytes.Repeat([]byte{'x'}, MaxFrame+1)); !errors.Is(err, ErrFrameSize) {
		t.Fatalf("error = %v, want ErrFrameSize", err)
	}
}

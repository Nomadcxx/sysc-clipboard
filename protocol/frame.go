package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var ErrFrameSize = errors.New("invalid protocol frame size")

func ReadFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaxFrame {
		return nil, fmt.Errorf("%w: %d", ErrFrameSize, size)
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func WriteFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > MaxFrame {
		return fmt.Errorf("%w: %d", ErrFrameSize, len(payload))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeFull(writer, header[:]); err != nil {
		return err
	}
	return writeFull(writer, payload)
}

func ReadMessage(reader io.Reader) (Message, error) {
	payload, err := ReadFrame(reader)
	if err != nil {
		return Message{}, err
	}
	if err := validateJSONDepth(payload); err != nil {
		return Message{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var message Message
	if err := decoder.Decode(&message); err != nil {
		return Message{}, fmt.Errorf("decode protocol message: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Message{}, errors.New("protocol message contains trailing JSON")
		}
		return Message{}, fmt.Errorf("decode trailing protocol JSON: %w", err)
	}
	if err := ValidateMessage(message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func WriteMessage(writer io.Writer, message Message) error {
	if err := ValidateMessage(message); err != nil {
		return err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode protocol message: %w", err)
	}
	return WriteFrame(writer, payload)
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if written < 0 || written > len(data) {
			return io.ErrShortWrite
		}
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func validateJSONDepth(payload []byte) error {
	depth := 0
	inString := false
	escaped := false
	for _, char := range payload {
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > MaxJSONDepth {
				return fmt.Errorf("JSON nesting exceeds %d", MaxJSONDepth)
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return nil
}

package wayland

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

var (
	errFDLimit      = errors.New("clipboard transfer exceeds its limit")
	errFDNoProgress = errors.New("clipboard transfer made no progress")
)

const (
	fdPollInterval = 25 * time.Millisecond
	fdChunkSize    = 32 << 10
)

func newTransferPipe() (reader, writer int, err error) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_CLOEXEC); err != nil {
		return -1, -1, err
	}
	return fds[0], fds[1], nil
}

func readFD(ctx context.Context, fd, limit int) (data []byte, err error) {
	if fd < 0 {
		return nil, fmt.Errorf("invalid clipboard read FD %d", fd)
	}
	defer unix.Close(fd)
	if limit < 0 {
		return nil, fmt.Errorf("invalid clipboard read limit %d", limit)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, err
	}

	data = make([]byte, 0, min(limit, fdChunkSize))
	buffer := make([]byte, fdChunkSize)
	for {
		if err := waitFD(ctx, fd, unix.POLLIN); err != nil {
			return nil, err
		}
		remaining := limit + 1 - len(data)
		if remaining <= 0 {
			return nil, errFDLimit
		}
		chunk := buffer
		if remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		n, readErr := unix.Read(fd, chunk)
		if n > 0 {
			data = append(data, chunk[:n]...)
			if len(data) > limit {
				return nil, errFDLimit
			}
		}
		if readErr == nil {
			if n == 0 {
				return data, nil
			}
			continue
		}
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) || errors.Is(readErr, unix.EWOULDBLOCK) {
			continue
		}
		return nil, readErr
	}
}

func writeFD(ctx context.Context, fd int, data []byte) error {
	if fd < 0 {
		return fmt.Errorf("invalid clipboard write FD %d", fd)
	}
	defer unix.Close(fd)
	if err := unix.SetNonblock(fd, true); err != nil {
		return err
	}
	for offset := 0; offset < len(data); {
		if err := waitFD(ctx, fd, unix.POLLOUT); err != nil {
			return err
		}
		n, err := unix.Write(fd, data[offset:])
		if n > 0 {
			offset += n
		}
		if err == nil {
			if n == 0 {
				return errFDNoProgress
			}
			continue
		}
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			continue
		}
		return err
	}
	return nil
}

func waitFD(ctx context.Context, fd int, events int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pollFD := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(pollFD, int(fdPollInterval/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		revents := pollFD[0].Revents
		if revents&unix.POLLNVAL != 0 {
			return fmt.Errorf("clipboard transfer FD is invalid")
		}
		if revents&(events|unix.POLLERR|unix.POLLHUP) != 0 {
			return nil
		}
	}
}

func closeFD(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}

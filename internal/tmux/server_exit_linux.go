//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"math"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func preparePIDWait(_ *Client, pid int) (pidWaiter, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return pidWaiter{wait: func(context.Context) error { return nil }, close: func() {}}, nil
	}
	if err != nil {
		return pidWaiter{}, fmt.Errorf("tmux: open server process: %w", err)
	}
	if fd < math.MinInt32 || fd > math.MaxInt32 {
		_ = unix.Close(fd)
		return pidWaiter{}, fmt.Errorf("tmux: server process descriptor %d exceeds poll range", fd)
	}
	pollFD := int32(fd)
	return pidWaiter{close: func() { _ = unix.Close(fd) }, wait: func(ctx context.Context) error {
		deadline, hasDeadline := ctx.Deadline()
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tmux: wait for server exit: %w", err)
			}
			timeout := 50
			if hasDeadline {
				timeout = min(timeout, max(0, int(time.Until(deadline).Milliseconds())))
			}
			n, err := unix.Poll([]unix.PollFd{{Fd: pollFD, Events: unix.POLLIN}}, timeout)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return fmt.Errorf("tmux: wait for server exit: %w", err)
			}
			if n > 0 {
				return nil
			}
			if timeout == 0 {
				return fmt.Errorf("tmux: server still running after kill-server: %w", context.DeadlineExceeded)
			}
		}
	}}, nil
}

//go:build darwin

package tmux

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func preparePIDWait(_ *Client, pid int) (pidWaiter, error) {
	fd, err := unix.Kqueue()
	if err != nil {
		return pidWaiter{}, fmt.Errorf("tmux: open server process observer: %w", err)
	}
	event := unix.Kevent_t{Fflags: unix.NOTE_EXIT}
	unix.SetKevent(&event, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	event.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(fd, []unix.Kevent_t{event}, nil, nil); err != nil {
		unix.Close(fd)
		if errors.Is(err, syscall.ESRCH) {
			return pidWaiter{wait: func(context.Context) error { return nil }, close: func() {}}, nil
		}
		return pidWaiter{}, fmt.Errorf("tmux: observe server process: %w", err)
	}
	return pidWaiter{close: func() { _ = unix.Close(fd) }, wait: func(ctx context.Context) error {
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("tmux: wait for server exit: %w", err)
			}
			wait := 50 * time.Millisecond
			if deadline, ok := ctx.Deadline(); ok {
				wait = min(wait, max(0, time.Until(deadline)))
			}
			timeout := unix.NsecToTimespec(wait.Nanoseconds())
			n, err := unix.Kevent(fd, nil, []unix.Kevent_t{{}}, &timeout)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return fmt.Errorf("tmux: wait for server exit: %w", err)
			}
			if n > 0 {
				return nil
			}
			if wait == 0 {
				return fmt.Errorf("tmux: server still running after kill-server: %w", context.DeadlineExceeded)
			}
		}
	}}, nil
}

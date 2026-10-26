//go:build !linux && !darwin

package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

func preparePIDWait(c *Client, pid int) (pidWaiter, error) {
	return pidWaiter{close: func() {}, wait: func(ctx context.Context) error {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			current, err := c.run(ctx, "display-message", "-p", "#{pid}")
			if errors.Is(err, ErrNoSession) || errors.Is(err, ErrNoServer) {
				return fmt.Errorf("tmux: cannot confirm server %d exited: %w", pid, err)
			}
			if err != nil {
				return err
			}
			if current != strconv.Itoa(pid) {
				return fmt.Errorf("tmux: cannot confirm server %d exited: socket now belongs to server %s", pid, current)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("tmux: server still running after kill-server: %w", ctx.Err())
			case <-ticker.C:
			}
		}
	}}, nil
}

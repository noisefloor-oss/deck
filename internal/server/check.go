package server

import (
	"context"
	"sync"
	"time"
)

type cachedValue[T any] struct {
	mu        sync.Mutex
	freshness time.Duration
	timeout   time.Duration
	load      func(context.Context) (T, error)
	now       func() time.Time

	checkedAt time.Time
	value     T
	err       error
	hasResult bool
	inFlight  chan struct{}
}

func newCachedValue[T any](freshness, timeout time.Duration,
	load func(context.Context) (T, error)) *cachedValue[T] {
	return &cachedValue[T]{
		freshness: freshness,
		timeout:   timeout,
		load:      load,
		now:       time.Now,
	}
}

func (c *cachedValue[T]) get(ctx context.Context) (T, error) {
	c.mu.Lock()
	now := c.now()
	if c.hasResult && now.Sub(c.checkedAt) < c.freshness {
		value, err := c.value, c.err
		c.mu.Unlock()
		return value, err
	}
	done := c.inFlight
	if done == nil {
		done = make(chan struct{})
		c.inFlight = done
		go c.execute(done)
	}
	if c.hasResult {
		// Stale-while-revalidate: serve the last completed result — failure
		// included — while one coalesced recheck runs on its own budget. A
		// caller's context can never outwait a provider CLI, and the recheck
		// still lands within freshness + one probe duration.
		value, err := c.value, c.err
		c.mu.Unlock()
		return value, err
	}
	c.mu.Unlock()

	// No completed result exists yet (cold start before priming lands): the
	// only honest answer comes from the in-flight probe, bounded by the
	// caller's context.
	select {
	case <-done:
		c.mu.Lock()
		value, err := c.value, c.err
		c.mu.Unlock()
		return value, err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (c *cachedValue[T]) execute(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	value, err := c.load(ctx)
	cancel()

	c.mu.Lock()
	c.value = value
	c.err = err
	c.checkedAt = c.now()
	c.hasResult = true
	c.inFlight = nil
	close(done)
	c.mu.Unlock()
}

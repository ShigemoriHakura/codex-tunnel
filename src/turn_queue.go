package main

import (
	"context"
	"slices"
	"sync"
)

// Each ticket becomes ready only after the previous turn has released the session.
// Removing a canceled waiter never releases a running turn or skips a live waiter.
type sessionTurns struct {
	mu    sync.Mutex
	lanes map[string][]chan struct{}
}

func (q *sessionTurns) join(id string, limit int) (<-chan struct{}, func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	lane := q.lanes[id]
	if len(lane) >= limit {
		return nil, nil
	}
	if q.lanes == nil {
		q.lanes = make(map[string][]chan struct{})
	}
	ready := make(chan struct{})
	q.lanes[id] = append(lane, ready)
	if len(lane) == 0 {
		close(ready)
	}
	return ready, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		lane := q.lanes[id]
		i := slices.Index(lane, ready)
		if i < 0 {
			return
		}
		lane = slices.Delete(lane, i, i+1)
		if len(lane) == 0 {
			delete(q.lanes, id)
			return
		}
		q.lanes[id] = lane
		if i == 0 {
			close(lane[0])
		}
	}
}

func (a *app) initWorkers(n int) {
	a.slots = make(chan struct{}, n)
	// Bound memory for requests being decoded or waiting with a validated payload.
	a.admitted = make(chan struct{}, n+min(2*n, 16))
}

func (a *app) waitForWorker(ctx context.Context, sessionReady <-chan struct{}) error {
	if sessionReady != nil {
		select {
		case <-sessionReady:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case a.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			a.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

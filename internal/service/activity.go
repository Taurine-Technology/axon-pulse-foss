package service

import (
	"context"
	"errors"
	"sync"
)

var (
	errActivityQuiescing = errors.New("service activity is quiescing for an update")
)

// activityGate closes admission before an update and waits only for work that
// was already admitted. It is separate from Service.wg because the automatic
// updater itself belongs to that wait group.
type (
	activityGate struct {
		mu        sync.Mutex
		active    int
		quiescing bool
		drained   chan struct{}
	}
)

func (g *activityGate) begin() (func(), bool) {
	g.mu.Lock()
	if g.quiescing {
		g.mu.Unlock()
		return nil, false
	}
	g.active++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			if g.quiescing && g.active == 0 {
				close(g.drained)
			}
			g.mu.Unlock()
		})
	}, true
}

func (g *activityGate) quiesce(ctx context.Context) (func(), error) {
	g.mu.Lock()
	if g.quiescing {
		g.mu.Unlock()
		return nil, errActivityQuiescing
	}
	g.quiescing = true
	g.drained = make(chan struct{})
	if g.active == 0 {
		close(g.drained)
	}
	drained := g.drained
	g.mu.Unlock()

	var resumeOnce sync.Once
	resume := func() {
		resumeOnce.Do(func() {
			g.mu.Lock()
			g.quiescing = false
			g.drained = nil
			g.mu.Unlock()
		})
	}
	select {
	case <-drained:
		if err := ctx.Err(); err != nil {
			resume()
			return nil, err
		}
		return resume, nil
	case <-ctx.Done():
		resume()
		return nil, ctx.Err()
	}
}

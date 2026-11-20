package jobs

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Processor interface {
	Verify(context.Context, []byte) ([]byte, error)
}
type Pool struct {
	store     *Store
	processor Processor
	timeout   time.Duration
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	wg        sync.WaitGroup
}

func NewPool(s *Store, p Processor, timeout time.Duration) *Pool {
	return &Pool{store: s, processor: p, timeout: timeout, active: make(map[string]context.CancelFunc)}
}
func (p *Pool) Start(ctx context.Context, n int) {
	for range n {
		p.wg.Add(1)
		go p.loop(ctx)
	}
}
func (p *Pool) Wait() { p.wg.Wait() }
func (p *Pool) Cancel(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.active[id]; f != nil {
		f()
	}
}
func (p *Pool) loop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		j, err := p.store.Claim()
		if err == nil {
			p.run(ctx, j)
			continue
		}
		if !errors.Is(err, ErrNotFound) {
			slog.Error("claim job", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (p *Pool) run(parent context.Context, j Job) {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	p.mu.Lock()
	p.active[j.ID] = cancel
	p.mu.Unlock()
	defer func() { cancel(); p.mu.Lock(); delete(p.active, j.ID); p.mu.Unlock() }()
	// Cancellation may have committed between Claim and registration above.
	current, err := p.store.Get(j.ID)
	if err != nil || current.Status != "running" {
		return
	}
	result, err := p.processor.Verify(ctx, j.Input)
	status, reason := "succeeded", ""
	if parent.Err() != nil {
		status = "queued"
		result = nil
	} else if ctx.Err() == context.DeadlineExceeded {
		status = "failed"
		reason = "verification_timeout"
		result = nil
	} else if err != nil {
		status = "failed"
		reason = "verification_failed"
		result = nil
	}
	if err = p.store.Finish(j.ID, status, reason, result); err != nil {
		slog.Error("finish job", "id", j.ID, "error", err)
	}
}

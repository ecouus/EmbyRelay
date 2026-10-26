package control

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// accessPacer turns the account-wide shared rate limiter into batched permits.
// One pace RPC grants a window of bytes; the bytes are then released locally at
// the plan rate. Permits start small so short responses do not hold the shared
// slot, double up to accessPaceBatch, and the next full-size permit is fetched
// one round trip early so RTT does not leave gaps in a long stream.
type accessPacer struct {
	ctx   context.Context
	rate  int64
	call  func(ctx context.Context, n int64) (time.Duration, error)
	limit atomic.Int64
	mu    sync.Mutex
	start time.Time
	size  int64
	used  int64
	last  int64
	lead  time.Duration
	ahead chan paceGrant
}
type paceGrant struct {
	at   time.Time
	size int64
	lead time.Duration
	err  error
}

func newAccessPacer(ctx context.Context, rate int64, call func(context.Context, int64) (time.Duration, error)) *accessPacer {
	p := &accessPacer{ctx: ctx, rate: rate, call: call}
	p.limit.Store(accessPaceBatch(rate))
	return p
}
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (p *accessPacer) span(n int64) time.Duration {
	return time.Duration(float64(n) / float64(p.rate) * float64(time.Second))
}

// Request one permit, waiting out any slot another stream of the account holds.
func (p *accessPacer) fetch(size int64) paceGrant {
	for {
		size = min(size, p.limit.Load())
		began := time.Now()
		wait, err := p.call(p.ctx, size)
		if err != nil {
			// A control that predates batched permits only accepts the minimum size.
			if small := int64(accessPaceChunk(p.rate)); size > small && strings.Contains(err.Error(), "invalid pace request") {
				p.limit.Store(small)
				continue
			}
			return paceGrant{err: err}
		}
		if wait <= 0 {
			return paceGrant{at: time.Now(), size: size, lead: time.Since(began)}
		}
		if err = sleepContext(p.ctx, wait); err != nil {
			return paceGrant{err: err}
		}
	}
}
func (p *accessPacer) nextSize() int64 {
	if p.last == 0 {
		p.last = int64(accessPaceChunk(p.rate))
	} else {
		p.last = min(p.last*2, p.limit.Load())
	}
	return p.last
}
func (p *accessPacer) advance() error {
	end := p.start.Add(p.span(p.size))
	var g paceGrant
	if p.ahead != nil {
		select {
		case g = <-p.ahead:
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
		p.ahead = nil
	} else {
		g = p.fetch(p.nextSize())
	}
	if g.err != nil {
		return g.err
	}
	p.lead, p.last = g.lead, g.size
	p.start = g.at
	if g.at.Before(end) {
		p.start = end
	}
	p.size, p.used = g.size, 0
	return nil
}

// take blocks until n bytes may be written, spreading them evenly over the permit.
func (p *accessPacer) take(n int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for n > 0 {
		if p.used >= p.size {
			if err := p.advance(); err != nil {
				return err
			}
		}
		m := min(int64(n), p.size-p.used)
		if d := time.Until(p.start.Add(p.span(p.used))); d > 0 {
			if err := sleepContext(p.ctx, d); err != nil {
				return err
			}
		}
		p.used += m
		n -= int(m)
		if p.ahead == nil && p.size >= p.limit.Load() && p.size-p.used <= int64(p.lead.Seconds()*float64(p.rate)) {
			p.ahead = make(chan paceGrant, 1)
			go func(ch chan paceGrant, size int64) { ch <- p.fetch(size) }(p.ahead, p.nextSize())
		}
	}
	return nil
}

// unusedNS is how long the account's shared slot is still booked for permits
// this request obtained but will not use. The finish call hands it back.
func (p *accessPacer) unusedNS() int64 {
	if !p.mu.TryLock() {
		return 0
	}
	defer p.mu.Unlock()
	end := p.start.Add(p.span(p.size))
	if p.ahead != nil {
		select {
		case g := <-p.ahead:
			if g.err == nil {
				if g.at.After(end) {
					end = g.at
				}
				end = end.Add(p.span(g.size))
			}
		default:
		}
	}
	return max(0, int64(time.Until(end)))
}

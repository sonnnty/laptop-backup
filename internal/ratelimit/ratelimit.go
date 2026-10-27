// Package ratelimit caps the combined speed of several upload streams with
// one token bucket.
package ratelimit

import (
	"context"
	"io"
	"sync"
	"time"
)

// burst is how much unused allowance an idle limiter saves up, in seconds
// of the configured rate.
const burst = 0.25

// Limiter is a token bucket shared by every reader it wraps. The zero value
// is unlimited and ready to use; the rate can be changed while readers are
// active.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second, 0 = unlimited
	tokens float64 // may go negative: readers sleep off the debt
	last   time.Time

	// now and sleep are replaced in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// SetRate sets the limit in bytes per second (<= 0 = unlimited). A changed
// rate starts with an empty bucket.
func (l *Limiter) SetRate(bytesPerSec int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rate := float64(max(bytesPerSec, 0))
	if rate == l.rate {
		return
	}
	l.rate, l.tokens, l.last = rate, 0, l.clock()
}

// Rate returns the current limit in bytes per second (0 = unlimited).
func (l *Limiter) Rate() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

func (l *Limiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// chunk is the most a single Read may return: at most 1/8 s of the rate,
// so that a rate change or a cancel takes effect quickly even with all
// workers waiting.
func (l *Limiter) chunk(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rate <= 0 {
		return n
	}
	return min(n, max(int(l.rate/8), 512))
}

// wait takes n bytes from the bucket and sleeps until they are paid for.
func (l *Limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := l.clock()
	l.tokens = min(l.tokens+now.Sub(l.last).Seconds()*l.rate, l.rate*burst)
	l.last = now
	l.tokens -= float64(n)
	d := time.Duration(-l.tokens / l.rate * float64(time.Second))
	sleep := l.sleep
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	if sleep != nil {
		return sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Reader returns r throttled by l; a nil l returns r unchanged. The rate is
// looked up on every Read, so changes apply to streams already open.
func (l *Limiter) Reader(ctx context.Context, r io.Reader) io.Reader {
	if l == nil {
		return r
	}
	return &reader{l: l, ctx: ctx, r: r}
}

type reader struct {
	l   *Limiter
	ctx context.Context
	r   io.Reader
}

func (r *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return r.r.Read(p)
	}
	n, err := r.r.Read(p[:r.l.chunk(len(p))])
	if n > 0 {
		if werr := r.l.wait(r.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

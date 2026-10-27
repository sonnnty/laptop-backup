package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when the limiter sleeps.
type fakeClock struct {
	t     time.Time
	slept time.Duration
}

func newFake(l *Limiter) *fakeClock {
	c := &fakeClock{t: time.Unix(1000, 0)}
	l.now = func() time.Time { return c.t }
	l.sleep = func(ctx context.Context, d time.Duration) error {
		c.t = c.t.Add(d)
		c.slept += d
		return ctx.Err()
	}
	return c
}

func copyN(t *testing.T, l *Limiter, ctx context.Context, n int) error {
	t.Helper()
	got, err := io.Copy(io.Discard, l.Reader(ctx, bytes.NewReader(make([]byte, n))))
	if err == nil && got != int64(n) {
		t.Fatalf("copied %d of %d bytes", got, n)
	}
	return err
}

func near(d, want time.Duration) bool { return d > want-want/20 && d < want+want/20 }

func TestRate(t *testing.T) {
	var l Limiter
	c := newFake(&l)
	l.SetRate(32 << 10)
	if err := copyN(t, &l, context.Background(), 64<<10); err != nil {
		t.Fatal(err)
	}
	if !near(c.slept, 2*time.Second) {
		t.Fatalf("64 KiB at 32 KiB/s took %v", c.slept)
	}
}

func TestIdleSavesOnlyABurst(t *testing.T) {
	var l Limiter
	c := newFake(&l)
	l.SetRate(32 << 10)
	c.t = c.t.Add(time.Minute) // idle: at most 1/4 s is saved up
	copyN(t, &l, context.Background(), 64<<10)
	if !near(c.slept, 1750*time.Millisecond) {
		t.Fatalf("slept %v", c.slept)
	}
}

func TestUnlimitedAndRateChange(t *testing.T) {
	var l Limiter
	c := newFake(&l)
	copyN(t, &l, context.Background(), 1<<20)
	if c.slept != 0 {
		t.Fatalf("unlimited limiter slept %v", c.slept)
	}

	// A reader opened while unlimited follows a later limit.
	l.SetRate(0)
	src := l.Reader(context.Background(), bytes.NewReader(make([]byte, 64<<10)))
	l.SetRate(64 << 10)
	io.Copy(io.Discard, src)
	if !near(c.slept, time.Second) {
		t.Fatalf("slept %v after rate change", c.slept)
	}
	if l.Rate() != 64<<10 {
		t.Fatalf("rate = %d", l.Rate())
	}
}

func TestCancel(t *testing.T) {
	var l Limiter
	newFake(&l)
	l.SetRate(1 << 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copyN(t, &l, ctx, 64<<10); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestNilLimiterPassesThrough(t *testing.T) {
	var l *Limiter
	r := bytes.NewReader(nil)
	if l.Reader(context.Background(), r) != io.Reader(r) {
		t.Fatal("nil limiter wrapped the reader")
	}
}

// TestSharedRealClock checks that concurrent readers share one budget,
// using the real clock: 4 × 8 KiB at 64 KiB/s needs about 0.5 s.
func TestSharedRealClock(t *testing.T) {
	var l Limiter
	l.SetRate(64 << 10)
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.Copy(io.Discard, l.Reader(context.Background(), bytes.NewReader(make([]byte, 8<<10))))
		}()
	}
	wg.Wait()
	if d := time.Since(start); d < 400*time.Millisecond || d > 3*time.Second {
		t.Fatalf("4 × 8 KiB at 64 KiB/s took %v", d)
	}
}

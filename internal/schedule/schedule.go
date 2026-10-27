// Package schedule computes when the next backup is due and runs a timer
// loop that triggers it.
package schedule

import (
	"context"
	"sync"
	"time"

	"laptop-backup/internal/config"
)

// Interval is the nominal gap between scheduled runs (0 when paused).
func Interval(s config.Schedule) time.Duration {
	switch s.Mode {
	case config.ModeInterval:
		return time.Duration(s.EveryHours) * time.Hour
	case config.ModeDaily:
		return 24 * time.Hour
	}
	return 0
}

// Next returns the next scheduled run time. lastRun is the start of the most
// recent run attempt (zero if none). ok is false when the schedule is paused.
// A result equal to now means "run immediately".
func Next(s config.Schedule, lastRun, now time.Time) (next time.Time, ok bool) {
	switch s.Mode {
	case config.ModeInterval:
		if lastRun.IsZero() {
			return now, true
		}
		next = lastRun.Add(Interval(s))
		if next.Before(now) {
			next = now
		}
		return next, true
	case config.ModeDaily:
		at, err := time.Parse("15:04", s.DailyAt)
		if err != nil {
			return time.Time{}, false
		}
		y, m, d := now.Date()
		next = time.Date(y, m, d, at.Hour(), at.Minute(), 0, 0, now.Location())
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		return next, true
	}
	return time.Time{}, false
}

// CatchUp reports whether a run should start shortly after the service
// starts: the last successful run is older than the schedule interval
// (e.g. the laptop was off or asleep at the scheduled time).
func CatchUp(s config.Schedule, lastSuccess, now time.Time) bool {
	iv := Interval(s)
	if iv == 0 {
		return false
	}
	return lastSuccess.IsZero() || now.Sub(lastSuccess) > iv
}

// CatchUpDelay is how long after startup a catch-up run begins; it also
// gives the network a moment to come up after boot or resume.
const CatchUpDelay = time.Minute

// AtStartup adjusts the first run after the service starts: a catch-up
// run starts CatchUpDelay after start, and no run starts earlier than that.
func AtStartup(next, started time.Time, catchUp bool) time.Time {
	if c := started.Add(CatchUpDelay); catchUp || next.Before(c) {
		return c
	}
	return next
}

// maxSleep caps each timer wait. Go timers run on CLOCK_MONOTONIC, which
// stops while the laptop is suspended, so a single long timer would fire late
// after a sleep; waking every few minutes to compare wall-clock time keeps
// the schedule honest at negligible cost.
const maxSleep = 10 * time.Minute

// Scheduler triggers Run according to the configured schedule.
type Scheduler struct {
	Schedule func() config.Schedule
	// LastRuns returns the start of the last run and the start of the last
	// successful run (zero values if none).
	LastRuns func() (lastStart, lastSuccess time.Time)
	// Run performs a backup and blocks until it finishes.
	Run func(ctx context.Context) error

	wake chan struct{}
	mu   sync.Mutex
	next time.Time
	ok   bool
}

// NextRun returns the currently planned run time.
func (s *Scheduler) NextRun() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next, s.ok
}

// Reset makes the loop recompute the next run (after a config change or a
// manual run).
func (s *Scheduler) Reset() {
	select {
	case s.wakeCh() <- struct{}{}:
	default:
	}
}

func (s *Scheduler) wakeCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

// Loop runs until ctx is cancelled.
func (s *Scheduler) Loop(ctx context.Context) {
	wake := s.wakeCh()
	started := time.Now()
	_, lastSuccess := s.LastRuns()
	catchUp := CatchUp(s.Schedule(), lastSuccess, started)
	firstRun := true
	var lastAttempt time.Time

	for {
		now := time.Now()
		sched := s.Schedule()
		lastStart, _ := s.LastRuns()
		if lastAttempt.After(lastStart) {
			lastStart = lastAttempt
		}
		if lastStart.After(started) {
			firstRun = false // a manual run already covered the catch-up
		}
		next, ok := Next(sched, lastStart, now)
		if ok && firstRun {
			next = AtStartup(next, started, catchUp)
		}
		s.mu.Lock()
		s.next, s.ok = next, ok
		s.mu.Unlock()

		var timerC <-chan time.Time
		var timer *time.Timer
		if ok {
			timer = time.NewTimer(min(max(next.Sub(now), 0), maxSleep))
			timerC = timer.C
		}
		fired := false
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timerC:
			fired = true
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		if !fired || time.Now().Before(next) {
			continue
		}
		firstRun = false
		lastAttempt = time.Now()
		_ = s.Run(ctx) // errors are logged and recorded by the runner
	}
}

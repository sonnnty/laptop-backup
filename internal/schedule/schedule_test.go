package schedule

import (
	"testing"
	"time"

	"laptop-backup/internal/config"
)

var bkk = time.FixedZone("ICT", 7*3600)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, bkk)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	every6 := config.Schedule{Mode: config.ModeInterval, EveryHours: 6}
	daily := config.Schedule{Mode: config.ModeDaily, DailyAt: "02:30"}
	paused := config.Schedule{Mode: config.ModePaused}
	tests := []struct {
		name   string
		s      config.Schedule
		last   time.Time
		now    time.Time
		want   time.Time
		wantOK bool
	}{
		{"interval never ran", every6, time.Time{}, at("2026-10-27 10:00"), at("2026-10-27 10:00"), true},
		{"interval upcoming", every6, at("2026-10-27 08:00"), at("2026-10-27 10:00"), at("2026-10-27 14:00"), true},
		{"interval overdue", every6, at("2026-10-26 08:00"), at("2026-10-27 10:00"), at("2026-10-27 10:00"), true},
		{"daily later today", daily, time.Time{}, at("2026-10-27 01:00"), at("2026-10-27 02:30"), true},
		{"daily tomorrow", daily, time.Time{}, at("2026-10-27 10:00"), at("2026-10-28 02:30"), true},
		{"daily exactly now", daily, time.Time{}, at("2026-10-27 02:30"), at("2026-10-28 02:30"), true},
		{"daily month end", daily, time.Time{}, at("2026-10-31 23:00"), at("2026-11-01 02:30"), true},
		{"paused", paused, time.Time{}, at("2026-10-27 10:00"), time.Time{}, false},
	}
	for _, tt := range tests {
		got, ok := Next(tt.s, tt.last, tt.now)
		if ok != tt.wantOK || !got.Equal(tt.want) {
			t.Errorf("%s: Next = %v %v, want %v %v", tt.name, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestCatchUp(t *testing.T) {
	every6 := config.Schedule{Mode: config.ModeInterval, EveryHours: 6}
	daily := config.Schedule{Mode: config.ModeDaily, DailyAt: "02:00"}
	now := at("2026-10-27 09:00")
	tests := []struct {
		name string
		s    config.Schedule
		last time.Time
		want bool
	}{
		{"never succeeded", every6, time.Time{}, true},
		{"recent", every6, at("2026-10-27 05:00"), false},
		{"laptop was off", every6, at("2026-10-27 01:00"), true},
		{"daily within a day", daily, at("2026-10-26 12:00"), false},
		{"daily missed", daily, at("2026-10-26 02:00"), true},
		{"paused never catches up", config.Schedule{Mode: config.ModePaused}, time.Time{}, false},
	}
	for _, tt := range tests {
		if got := CatchUp(tt.s, tt.last, now); got != tt.want {
			t.Errorf("%s: CatchUp = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAtStartup(t *testing.T) {
	start := at("2026-10-27 09:00")
	soon := start.Add(CatchUpDelay)
	if got := AtStartup(at("2026-10-27 15:00"), start, true); !got.Equal(soon) {
		t.Errorf("catch-up: %v", got)
	}
	if got := AtStartup(start, start, false); !got.Equal(soon) {
		t.Errorf("due now: %v", got)
	}
	if got := AtStartup(at("2026-10-27 15:00"), start, false); !got.Equal(at("2026-10-27 15:00")) {
		t.Errorf("not due: %v", got)
	}
}

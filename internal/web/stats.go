package web

import (
	"context"
	"os"
	"runtime"
	"slices"
	stdsync "sync"
	"time"

	"laptop-backup/internal/index"
)

// Windows of the dashboard charts.
const (
	statsDays = 30 // bytes-per-day chart and the "runs in 30 days" tile
	statsRuns = 30 // runs shown in the per-run charts
)

// Cache lifetimes. The per-source sums scan the whole files index (100k+
// rows), so they are recomputed only when a run has finished or the source
// list changed; while a run is in progress (the index is changing) at most
// once per sourceStatsEvery. Drive's storage quota changes slowly and costs
// an API call.
const (
	sourceStatsEvery = time.Minute
	quotaTTL         = 10 * time.Minute
	quotaErrorTTL    = time.Minute // retry a failed quota call this soon
	defaultQuotaWait = 2 * time.Second
)

// Quota states in /api/stats.
const (
	quotaOK           = "ok"
	quotaError        = "error"
	quotaNotConnected = "not_connected"
)

type dayStat struct {
	Day   string `json:"day"` // local date, YYYY-MM-DD
	Bytes int64  `json:"bytes"`
}

type runStat struct {
	ID          int64  `json:"id"`
	Started     string `json:"started"` // local, as in the history table
	StartedUnix int64  `json:"started_unix"`
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	Uploaded    int    `json:"uploaded"`
	Updated     int    `json:"updated"`
	Trashed     int    `json:"trashed"`
	Errors      int    `json:"errors"`
	Bytes       int64  `json:"bytes"`
	DurationSec int64  `json:"duration_sec"`
}

type sourceStat struct {
	Path    string `json:"path"`
	Display string `json:"display"`
	Files   int64  `json:"files"`
	Bytes   int64  `json:"bytes"`
}

type quotaStat struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"` // Thai text for the error states
	// Limit is 0 with Unlimited set for accounts without a storage cap.
	Limit     int64 `json:"limit"`
	Unlimited bool  `json:"unlimited"`
	Usage     int64 `json:"usage"`
	InDrive   int64 `json:"in_drive"`
	InTrash   int64 `json:"in_trash"`
	// Stale marks a cached value shown because the latest call failed.
	Stale   bool   `json:"stale,omitempty"`
	Fetched string `json:"fetched,omitempty"` // RFC 3339
}

type tileStats struct {
	Files       int64    `json:"files"` // every file in the index
	Bytes       int64    `json:"bytes"`
	Runs        int      `json:"runs"` // finished runs in the last statsDays days
	Success     int      `json:"success"`
	SuccessRate *float64 `json:"success_rate"` // nil when there were no runs
	HeapInuse   uint64   `json:"heap_inuse"`
	Sys         uint64   `json:"sys"`
	DBBytes     int64    `json:"db_bytes"`
}

type statsResponse struct {
	OK        bool         `json:"ok"`
	Generated string       `json:"generated"`
	Tiles     tileStats    `json:"tiles"`
	Daily     []dayStat    `json:"daily"`
	Runs      []runStat    `json:"runs"`
	Sources   []sourceStat `json:"sources"`
	Quota     quotaStat    `json:"quota"`
}

// statsCache holds the two expensive parts of /api/stats.
type statsCache struct {
	mu stdsync.Mutex

	// Index scan: valid for the source list in srcKey and the run in srcRun.
	srcOK   bool
	srcKey  []string
	srcRun  int64
	srcAt   time.Time
	sources []sourceStat
	total   index.Usage

	// Drive quota.
	quota      quotaStat
	quotaAt    time.Time
	quotaFor   time.Duration // how long quota stays fresh (shorter after an error)
	quotaOwner string        // account the value belongs to
	fetching   bool
}

// scan returns the per-source and total usage, rescanning the index only
// when needed (see sourceStatsEvery).
func (s *Server) scan(sources []string, running bool) ([]sourceStat, index.Usage, error) {
	c := &s.stats
	var lastRun int64
	if runs, err := s.Index.FinishedRuns(1); err != nil {
		return nil, index.Usage{}, err
	} else if len(runs) > 0 {
		lastRun = runs[0].ID
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := c.srcOK && c.srcRun == lastRun && slices.Equal(c.srcKey, sources) &&
		(!running || time.Since(c.srcAt) < sourceStatsEvery)
	if fresh {
		return c.sources, c.total, nil
	}
	out := make([]sourceStat, 0, len(sources))
	for _, src := range sources {
		u, err := s.Index.SourceUsage(src)
		if err != nil {
			return nil, index.Usage{}, err
		}
		out = append(out, sourceStat{Path: src, Display: s.tilde(src), Files: u.Files, Bytes: u.Bytes})
	}
	total, err := s.Index.TotalUsage()
	if err != nil {
		return nil, index.Usage{}, err
	}
	c.srcOK, c.srcKey, c.srcRun, c.srcAt = true, slices.Clone(sources), lastRun, time.Now()
	c.sources, c.total = out, total
	return out, total, nil
}

// quotaInfo returns the cached Drive quota, fetching it (with a short
// timeout) when it is older than quotaTTL. A slow or failing Drive never
// blocks the page for long: the previous value (marked stale) or an error
// state is returned instead.
func (s *Server) quotaInfo(ctx context.Context) quotaStat {
	st := s.Auth.Status()
	if !st.Connected {
		return quotaStat{State: quotaNotConnected, Message: "ยังไม่ได้เชื่อมต่อ Google"}
	}
	c := &s.stats
	c.mu.Lock()
	if c.quotaOwner != st.Email { // reconnected with another account
		c.quota, c.quotaAt, c.quotaOwner = quotaStat{}, time.Time{}, st.Email
	}
	if !c.quotaAt.IsZero() && time.Since(c.quotaAt) < c.quotaFor {
		q := c.quota
		c.mu.Unlock()
		return q
	}
	if c.fetching { // another request is already asking Drive
		q := c.quota
		c.mu.Unlock()
		return staleOrError(q)
	}
	c.fetching = true
	prev := c.quota
	c.mu.Unlock()

	wait := s.QuotaWait
	if wait == 0 {
		wait = defaultQuotaWait
	}
	fctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	q, err := s.fetchQuota(fctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching = false
	if err != nil {
		s.Log.Warn("drive storage quota", "err", err)
		// Keep the last good value, and look again after quotaErrorTTL.
		c.quota, c.quotaAt, c.quotaFor = staleOrError(prev), time.Now(), quotaErrorTTL
		return c.quota
	}
	c.quota, c.quotaAt, c.quotaFor = q, time.Now(), quotaTTL
	return q
}

func staleOrError(prev quotaStat) quotaStat {
	if prev.State == quotaOK {
		prev.Stale = true
		return prev
	}
	return quotaStat{State: quotaError, Message: "ไม่สามารถดึงข้อมูลพื้นที่ได้"}
}

func (s *Server) fetchQuota(ctx context.Context) (quotaStat, error) {
	cl, err := s.NewDrive(ctx)
	if err != nil {
		return quotaStat{}, err
	}
	dq, err := cl.Quota(ctx)
	if err != nil {
		return quotaStat{}, err
	}
	return quotaStat{State: quotaOK, Limit: dq.Limit, Unlimited: dq.Limit == 0, Usage: dq.Usage,
		InDrive: dq.InDrive, InTrash: dq.InTrash, Fetched: time.Now().Format(time.RFC3339)}, nil
}

// dbSize is the size of the state database on disk, including its WAL.
func (s *Server) dbSize() int64 {
	var n int64
	for _, name := range []string{s.Paths.StateDB(), s.Paths.StateDB() + "-wal"} {
		if info, err := os.Stat(name); err == nil {
			n += info.Size()
		}
	}
	return n
}

// buildStats gathers everything the dashboard charts show.
func (s *Server) buildStats(ctx context.Context, now time.Time) (statsResponse, error) {
	resp := statsResponse{OK: true, Generated: now.Format(time.RFC3339)}

	days, err := s.Index.BytesPerDay(now, statsDays)
	if err != nil {
		return resp, err
	}
	resp.Daily = make([]dayStat, len(days))
	for i, d := range days {
		resp.Daily[i] = dayStat{Day: d.Day.Format("2006-01-02"), Bytes: d.Bytes}
	}

	runs, err := s.Index.FinishedRuns(statsRuns)
	if err != nil {
		return resp, err
	}
	resp.Runs = make([]runStat, len(runs))
	for i, r := range runs {
		resp.Runs[i] = runStat{
			ID: r.ID, Started: fmtTime(r.Started), StartedUnix: r.Started.Unix(),
			Status: r.Status, StatusLabel: statusLabel(r.Status),
			Uploaded: r.Uploaded, Updated: r.Updated, Trashed: r.Trashed, Errors: r.Errors,
			Bytes: r.Bytes, DurationSec: int64(r.Duration().Seconds()),
		}
	}

	counts, err := s.Index.RunCounts(days[0].Day)
	if err != nil {
		return resp, err
	}
	t := &resp.Tiles
	for _, n := range counts {
		t.Runs += n
	}
	t.Success = counts[index.StatusSuccess]
	if t.Runs > 0 {
		rate := float64(t.Success) / float64(t.Runs)
		t.SuccessRate = &rate
	}

	sources, total, err := s.scan(s.Config.Get().Sources, s.Runner.Status().Running)
	if err != nil {
		return resp, err
	}
	resp.Sources = sources
	t.Files, t.Bytes = total.Files, total.Bytes

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.HeapInuse, t.Sys = m.HeapInuse, m.Sys
	t.DBBytes = s.dbSize()

	resp.Quota = s.quotaInfo(ctx)
	return resp, nil
}

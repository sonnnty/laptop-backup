package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/drive/drivetest"
	"laptop-backup/internal/index"
)

func getStats(t *testing.T, h http.Handler) statsResponse {
	t.Helper()
	w := do(h, "GET", "/api/stats", "", false)
	var r statsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); w.Code != 200 || err != nil || !r.OK {
		t.Fatalf("GET /api/stats = %d %v: %s", w.Code, err, w.Body)
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", w.Header())
	}
	return r
}

func seedRun(t *testing.T, s *Server, started time.Time, status string, up, upd, tr, errs int, bytes int64, dur time.Duration) {
	t.Helper()
	id, err := s.Index.StartRun("schedule", started)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Index.FinishRun(index.Run{ID: id, Status: status, Finished: started.Add(dur),
		Uploaded: up, Updated: upd, Trashed: tr, Errors: errs, Bytes: bytes})
	if err != nil {
		t.Fatal(err)
	}
}

func putFile(t *testing.T, s *Server, path string, size int64) {
	t.Helper()
	if err := s.Index.Put(index.Entry{Path: path, DriveID: "id" + path, Size: size}, 1); err != nil {
		t.Fatal(err)
	}
}

func TestAPIStatsShape(t *testing.T) {
	s, h := newServer(t)
	home := s.Paths.Home
	a, b := home+"/a/b", home+"/a/bc"
	s.Config.Update(func(c *config.Config) { c.Sources = []string{a, b, home + "/empty"} })
	putFile(t, s, a+"/x", 100)
	putFile(t, s, a+"/sub/y", 20)
	putFile(t, s, b+"/z", 5000) // sibling prefix: only counts for bc
	s.Index.Put(index.Entry{Path: a + "/sub", DriveID: "d", IsDir: true, Size: 4096}, 1)

	now := time.Now()
	seedRun(t, s, now.AddDate(0, 0, -40), index.StatusSuccess, 9, 9, 9, 0, 777, time.Minute) // run 1: outside 30 days
	seedRun(t, s, now.Add(-2*time.Hour), index.StatusSuccess, 3, 2, 1, 0, 1000, 90*time.Second)
	seedRun(t, s, now.Add(-time.Hour), index.StatusPartial, 1, 0, 0, 2, 500, time.Minute)
	s.Index.StartRun("manual", now) // running: excluded everywhere

	r := getStats(t, h)

	if len(r.Daily) != statsDays || r.Daily[statsDays-1].Day != now.Format("2006-01-02") || r.Daily[statsDays-1].Bytes != 1500 {
		t.Errorf("daily = %+v", r.Daily[statsDays-3:])
	}
	var sum int64
	for _, d := range r.Daily {
		sum += d.Bytes
	}
	if sum != 1500 {
		t.Errorf("daily total %d, the 40-day-old run must not count", sum)
	}
	if len(r.Runs) != 3 || r.Runs[2].Status != index.StatusPartial || r.Runs[2].Errors != 2 || r.Runs[1].DurationSec != 90 ||
		r.Runs[1].Uploaded != 3 || r.Runs[0].ID > r.Runs[1].ID || r.Runs[2].StatusLabel != "สำเร็จบางส่วน" {
		t.Errorf("runs = %+v", r.Runs)
	}
	if len(r.Sources) != 3 || r.Sources[0].Files != 2 || r.Sources[0].Bytes != 120 || r.Sources[0].Display != "~/a/b" ||
		r.Sources[1].Files != 1 || r.Sources[1].Bytes != 5000 || r.Sources[2].Files != 0 {
		t.Errorf("sources = %+v", r.Sources)
	}
	tl := r.Tiles
	if tl.Files != 3 || tl.Bytes != 5120 || tl.Runs != 2 || tl.Success != 1 || tl.SuccessRate == nil || *tl.SuccessRate != 0.5 {
		t.Errorf("tiles = %+v", tl)
	}
	if tl.HeapInuse == 0 || tl.Sys < tl.HeapInuse || tl.DBBytes <= 0 {
		t.Errorf("memory/db tiles = %+v", tl)
	}
	if r.Quota.State != quotaNotConnected || r.Quota.Message != "ยังไม่ได้เชื่อมต่อ Google" {
		t.Errorf("quota = %+v", r.Quota)
	}
}

func TestAPIStatsEmpty(t *testing.T) {
	_, h := newServer(t)
	w := do(h, "GET", "/api/stats", "", false)
	// Lists are [] and the success rate null, never missing, so the JS can iterate.
	for _, want := range []string{`"runs":[]`, `"sources":[]`, `"success_rate":null`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("missing %s in %s", want, w.Body)
		}
	}
	r := getStats(t, h)
	if len(r.Daily) != statsDays || r.Tiles.Runs != 0 || r.Tiles.Files != 0 {
		t.Errorf("empty stats = %+v", r)
	}
}

func TestAPIStatsCachesIndexScan(t *testing.T) {
	s, h := newServer(t)
	src := s.Paths.Home + "/docs"
	s.Config.Update(func(c *config.Config) { c.Sources = []string{src} })
	putFile(t, s, src+"/a", 10)
	if got := getStats(t, h).Sources[0]; got.Files != 1 || got.Bytes != 10 {
		t.Fatalf("first = %+v", got)
	}

	// Nothing finished and the sources are unchanged: the cached scan is served.
	putFile(t, s, src+"/b", 5)
	if got := getStats(t, h); got.Sources[0].Files != 1 || got.Tiles.Files != 1 {
		t.Fatalf("index was rescanned without a trigger: %+v", got.Sources)
	}
	// A finished run invalidates it.
	seedRun(t, s, time.Now(), index.StatusSuccess, 1, 0, 0, 0, 5, time.Second)
	if got := getStats(t, h); got.Sources[0].Files != 2 || got.Sources[0].Bytes != 15 {
		t.Fatalf("after run = %+v", got.Sources)
	}
	// A changed source list does too.
	putFile(t, s, src+"2/c", 1)
	s.Config.Update(func(c *config.Config) { c.Sources = []string{src, src + "2"} })
	if got := getStats(t, h); len(got.Sources) != 2 || got.Sources[1].Bytes != 1 {
		t.Fatalf("after source change = %+v", got.Sources)
	}
}

// connectedWithFake returns a server that is signed in to a fake Drive.
func connectedWithFake(t *testing.T) (*Server, http.Handler, *drivetest.Fake) {
	t.Helper()
	s, h := newServerWith(t, fakeConnected)
	fake := drivetest.New(testFolderID)
	s.NewDrive = func(context.Context) (drive.Client, error) { return fake, nil }
	return s, h, fake
}

func TestAPIStatsQuota(t *testing.T) {
	_, h, fake := connectedWithFake(t)
	fake.QuotaValue = drive.Quota{Limit: 15 << 30, Usage: 5 << 30, InDrive: 4 << 30, InTrash: 1 << 30}
	q := getStats(t, h).Quota
	if q.State != quotaOK || q.Limit != 15<<30 || q.Usage != 5<<30 || q.InDrive != 4<<30 || q.InTrash != 1<<30 || q.Unlimited || q.Stale {
		t.Fatalf("quota = %+v", q)
	}
	// Cached for 10 minutes: the second page view does not call Drive.
	fake.QuotaValue.Usage = 9 << 30
	if q := getStats(t, h).Quota; q.Usage != 5<<30 || fake.Quotas != 1 {
		t.Fatalf("not cached: %+v after %d calls", q, fake.Quotas)
	}
}

func TestAPIStatsQuotaUnlimited(t *testing.T) {
	_, h, fake := connectedWithFake(t)
	fake.QuotaValue = drive.Quota{Usage: 1 << 30, InDrive: 1 << 30} // limit absent
	if q := getStats(t, h).Quota; q.State != quotaOK || !q.Unlimited || q.Limit != 0 {
		t.Fatalf("quota = %+v", q)
	}
}

func TestAPIStatsQuotaError(t *testing.T) {
	s, h, fake := connectedWithFake(t)
	fake.QuotaErr = errors.New("boom: secret detail")
	w := do(h, "GET", "/api/stats", "", false)
	var r statsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); w.Code != 200 || err != nil || !r.OK {
		t.Fatalf("stats must still work: %d %s", w.Code, w.Body)
	}
	if r.Quota.State != quotaError || r.Quota.Message != "ไม่สามารถดึงข้อมูลพื้นที่ได้" || strings.Contains(w.Body.String(), "secret detail") {
		t.Fatalf("quota = %+v", r.Quota)
	}
	if len(r.Daily) != statsDays {
		t.Fatal("the rest of the stats is missing")
	}
	// Errors are cached briefly so a broken Drive is not hammered.
	getStats(t, h)
	if fake.Quotas != 1 {
		t.Fatalf("%d Drive calls", fake.Quotas)
	}

	// Recovery after the error TTL.
	fake.QuotaErr, fake.QuotaValue = nil, drive.Quota{Limit: 100, Usage: 40}
	s.stats.quotaAt = time.Now().Add(-2 * quotaErrorTTL)
	if q := getStats(t, h).Quota; q.State != quotaOK || q.Usage != 40 {
		t.Fatalf("after recovery: %+v", q)
	}
	// A later failure keeps showing the last good value, marked stale.
	fake.QuotaErr = errors.New("down")
	s.stats.quotaAt = time.Now().Add(-2 * quotaTTL)
	if q := getStats(t, h).Quota; q.State != quotaOK || !q.Stale || q.Usage != 40 {
		t.Fatalf("stale quota = %+v", q)
	}
}

func TestAPIStatsSlowDriveDoesNotBlock(t *testing.T) {
	s, h, fake := connectedWithFake(t)
	fake.Latency = 10 * time.Second
	s.QuotaWait = 50 * time.Millisecond
	start := time.Now()
	q := getStats(t, h).Quota
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("/api/stats waited %v for Drive", d)
	}
	if q.State != quotaError {
		t.Fatalf("quota = %+v", q)
	}
}

func TestAPIStatsGuard(t *testing.T) {
	_, h := newServer(t)
	r := httptest.NewRequest("GET", "/api/stats", nil)
	r.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", w.Code)
	}
	if w := do(h, "POST", "/api/stats", "{}", true); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/stats: %d", w.Code)
	}
}

func TestDashboardUsageSection(t *testing.T) {
	_, h := newServer(t)
	dash := do(h, "GET", "/", "", false).Body.String()
	for _, want := range []string{`id="usage"`, `data-chart="daily"`, `data-chart="changes"`, `data-chart="durations"`,
		`data-chart="sources"`, `data-chart="quota"`, `data-chart="speed"`, `data-tile="files"`, `data-tile="db"`,
		`<script src="/static/charts.js">`} {
		if !strings.Contains(dash, want) {
			t.Errorf("dashboard lacks %s", want)
		}
	}
	if strings.Index(dash, "/static/charts.js") > strings.Index(dash, "/static/app.js") {
		t.Error("charts.js must load before app.js")
	}
	if hist := do(h, "GET", "/history", "", false).Body.String(); strings.Contains(hist, "charts.js") {
		t.Error("history page loads the chart code")
	}
	w := do(h, "GET", "/static/charts.js", "", false)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Body.String(), "const Charts") {
		t.Fatalf("charts.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	// Chart text comes from API data: it must never be parsed as markup.
	js := do(h, "GET", "/static/charts.js", "", false).Body.String()
	if strings.Contains(js, "innerHTML") || strings.Contains(js, "insertAdjacentHTML") {
		t.Error("charts.js builds markup from strings")
	}
	// The SVG namespace is an identifier, not a request.
	if strings.Contains(js, "https://") || strings.Contains(strings.ReplaceAll(js, "http://www.w3.org/2000/svg", ""), "http://") {
		t.Error("charts.js references an external URL")
	}
}

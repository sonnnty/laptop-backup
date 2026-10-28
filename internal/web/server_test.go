package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"laptop-backup/internal/auth"
	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/drive/drivetest"
	"laptop-backup/internal/index"
	"laptop-backup/internal/schedule"
	bsync "laptop-backup/internal/sync"
)

const testFolderID = "testDriveFolder_0123456789"

func newServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	return newServerWith(t, nil)
}

// newServerWith lets setup write files (e.g. a fake OAuth token) into the
// config dir before the server loads them.
func newServerWith(t *testing.T, setup func(config.Paths)) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Home: dir, ConfigDir: filepath.Join(dir, "cfg"), DataDir: filepath.Join(dir, "data")}
	if setup != nil {
		os.MkdirAll(paths.ConfigDir, 0o700)
		setup(paths)
	}
	store, _ := config.Load(paths.ConfigFile())
	idx, err := index.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	am, err := auth.New(paths.CredentialsFile(), paths.TokenFile())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := drivetest.New(testFolderID)
	newDrive := func(context.Context) (drive.Client, error) { return fake, nil }
	sched := &schedule.Scheduler{
		Schedule: func() config.Schedule { return store.Get().Schedule },
		LastRuns: func() (time.Time, time.Time) { return time.Time{}, time.Time{} },
	}
	s := &Server{
		Config: store, Auth: am, Index: idx, Paths: paths, Log: log, Sched: sched, NewDrive: newDrive,
		Runner: &bsync.Runner{Index: idx, Config: store, Home: dir, Log: log, NewDrive: newDrive, BaseCtx: context.Background()},
		Hosts:  LoopbackHosts("127.0.0.1:8095", "backup"),
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return s, h
}

func do(h http.Handler, method, target, body string, header bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = "backup"
	if header {
		r.Header.Set("X-Backup-UI", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPagesRenderWhenNotConnected(t *testing.T) {
	_, h := newServer(t)
	for _, p := range []string{"/", "/history", "/folders", "/settings", "/logs", "/static/app.js"} {
		w := do(h, "GET", p, "", false)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d: %s", p, w.Code, w.Body)
		}
	}
	body := do(h, "GET", "/", "", false).Body.String()
	if !strings.Contains(body, "not connected") || !strings.Contains(body, "credentials.json") {
		t.Fatalf("dashboard lacks not-connected state:\n%s", body)
	}
}

func TestGuard(t *testing.T) {
	_, h := newServer(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %d", w.Code)
	}
	if w := do(h, "POST", "/api/cancel", "{}", false); w.Code != http.StatusForbidden {
		t.Fatalf("POST without header: %d", w.Code)
	}
	if got := LoopbackHosts("127.0.0.1:8095", ""); slices.Contains(got, "backup") || !slices.Contains(got, "127.0.0.1:8095") {
		t.Fatalf("LoopbackHosts without proxy = %q", got)
	}
	if got := LoopbackHosts("127.0.0.1:8095", "nas"); !slices.Contains(got, "nas") {
		t.Fatalf("LoopbackHosts with proxy = %q", got)
	}
}

func TestFoldersExcludesAndSchedule(t *testing.T) {
	s, h := newServer(t)
	src := filepath.Join(s.Paths.Home, "Docs")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(src, "b.tmp"), []byte("x"), 0o644)

	if w := do(h, "POST", "/api/folders/add", `{"path":"`+src+`"}`, true); w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if got := s.Config.Get().Sources; len(got) != 1 || got[0] != src {
		t.Fatalf("sources = %q", got)
	}
	if w := do(h, "POST", "/api/folders/add", `{"path":"relative"}`, true); w.Code != 400 {
		t.Fatalf("relative path accepted: %d", w.Code)
	}
	w := do(h, "GET", "/api/preview?path="+src, "", false)
	if !strings.Contains(w.Body.String(), `"files":1`) {
		t.Fatalf("preview = %s", w.Body)
	}
	w = do(h, "GET", "/api/browse?path="+s.Paths.Home, "", false)
	if !strings.Contains(w.Body.String(), `"Docs"`) {
		t.Fatalf("browse = %s", w.Body)
	}

	if w := do(h, "POST", "/api/excludes", `{"patterns":"*.bak\n\nbuild/\n"}`, true); w.Code != 200 {
		t.Fatalf("excludes: %d %s", w.Code, w.Body)
	}
	if got := s.Config.Get().Excludes; len(got) != 2 || got[1] != "build/" {
		t.Fatalf("excludes = %q", got)
	}
	if w := do(h, "POST", "/api/excludes", `{"patterns":"[bad"}`, true); w.Code != 400 {
		t.Fatalf("bad pattern accepted: %d", w.Code)
	}
	// Comment lines are kept but not validated as patterns.
	if w := do(h, "POST", "/api/excludes", `{"patterns":"# [old] build dirs\nbuild/\n"}`, true); w.Code != 200 {
		t.Fatalf("comment rejected: %d %s", w.Code, w.Body)
	}
	if got := s.Config.Get().Excludes; len(got) != 2 || got[0] != "# [old] build dirs" {
		t.Fatalf("excludes = %q", got)
	}

	if w := do(h, "POST", "/api/schedule", `{"mode":"daily","daily_at":"03:15"}`, true); w.Code != 200 {
		t.Fatalf("schedule: %d %s", w.Code, w.Body)
	}
	if sch := s.Config.Get().Schedule; sch.Mode != "daily" || sch.DailyAt != "03:15" || sch.EveryHours != 6 {
		t.Fatalf("schedule = %+v", sch)
	}
	if w := do(h, "POST", "/api/schedule", `{"mode":"interval","every_hours":25}`, true); w.Code != 400 {
		t.Fatalf("25h accepted: %d", w.Code)
	}

	if w := do(h, "POST", "/api/folders/remove", `{"path":"`+src+`"}`, true); w.Code != 200 || len(s.Config.Get().Sources) != 0 {
		t.Fatalf("remove: %d %q", w.Code, s.Config.Get().Sources)
	}
	// Config was persisted.
	if data, _ := os.ReadFile(s.Paths.ConfigFile()); !strings.Contains(string(data), `"03:15"`) {
		t.Fatalf("config.json = %s", data)
	}
}

func TestUploadSettings(t *testing.T) {
	s, h := newServer(t)
	body := do(h, "GET", "/settings", "", false).Body.String()
	if !strings.Contains(body, "จำนวนไฟล์ที่อัปโหลดพร้อมกัน") || !strings.Contains(body, `name="upload_workers" min="1" max="8" value="4"`) {
		t.Fatalf("settings page lacks upload form:\n%s", body)
	}

	if w := do(h, "POST", "/api/upload", `{"upload_workers":6,"upload_limit_kbps":256}`, true); w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	if c := s.Config.Get(); c.UploadWorkers != 6 || c.UploadLimitKBps != 256 {
		t.Fatalf("config = %d workers, %d KB/s", c.UploadWorkers, c.UploadLimitKBps)
	}
	if s.Runner.UploadLimit() != 256<<10 {
		t.Fatalf("limit not applied: %d", s.Runner.UploadLimit())
	}
	if data, _ := os.ReadFile(s.Paths.ConfigFile()); !strings.Contains(string(data), `"upload_limit_kbps": 256`) {
		t.Fatalf("config.json = %s", data)
	}

	for _, bad := range []string{
		`{"upload_workers":0,"upload_limit_kbps":0}`,
		`{"upload_workers":9,"upload_limit_kbps":0}`,
		`{"upload_workers":4,"upload_limit_kbps":-1}`,
		`{"upload_limit_kbps":100}`,
		`{"upload_workers":"4"}`,
	} {
		if w := do(h, "POST", "/api/upload", bad, true); w.Code != 400 {
			t.Fatalf("%s accepted: %d", bad, w.Code)
		}
	}
	if c := s.Config.Get(); c.UploadWorkers != 6 || c.UploadLimitKBps != 256 || s.Runner.UploadLimit() != 256<<10 {
		t.Fatalf("rejected request changed settings: %+v", c)
	}
}

func TestRunRequiresConnection(t *testing.T) {
	s, h := newServer(t)
	s.Config.Update(func(c *config.Config) { c.Sources = []string{s.Paths.Home} })
	w := do(h, "POST", "/api/run", `{}`, true)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Google") {
		t.Fatalf("run without Google: %d %s", w.Code, w.Body)
	}
}

func TestDriveFolderTest(t *testing.T) {
	_, h := newServer(t)
	w := do(h, "POST", "/api/drive-folder/test", `{"folder_id":"`+testFolderID+`"}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"root"`) {
		t.Fatalf("test access: %d %s", w.Code, w.Body)
	}
	w = do(h, "POST", "/api/drive-folder/test", `{"folder_id":"doesNotExist_123"}`, true)
	if w.Code == 200 {
		t.Fatalf("missing folder passed: %s", w.Body)
	}
}

// fakeConnected writes an OAuth client and a refresh token so the auth
// manager reports "connected" without talking to Google.
func fakeConnected(p config.Paths) {
	os.WriteFile(p.CredentialsFile(), []byte(`{"client_id":"id","client_secret":"secret"}`), 0o600)
	os.WriteFile(p.TokenFile(), []byte(`{"refresh_token":"rt","email":"alice@example.com"}`), 0o600)
}

func TestRunRefusedWithoutDriveFolder(t *testing.T) {
	s, h := newServerWith(t, fakeConnected)
	if !s.Auth.Status().Connected {
		t.Fatal("fake token not loaded")
	}
	for _, p := range []string{"/", "/settings"} {
		if body := do(h, "GET", p, "", false).Body.String(); !strings.Contains(body, "ยังไม่ได้ตั้งค่าโฟลเดอร์ปลายทางบน Google Drive") {
			t.Fatalf("%s lacks the no-folder notice:\n%s", p, body)
		}
	}
	s.Config.Update(func(c *config.Config) { c.Sources = []string{s.Paths.Home} })
	w := do(h, "POST", "/api/run", `{}`, true)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "โฟลเดอร์ปลายทาง") {
		t.Fatalf("run without folder: %d %s", w.Code, w.Body)
	}
	if s.Runner.Status().Running {
		t.Fatal("run started")
	}
	if w := do(h, "POST", "/api/drive-folder", `{"folder_id":" "}`, true); w.Code != 400 {
		t.Fatalf("empty folder ID saved: %d", w.Code)
	}

	if w := do(h, "POST", "/api/drive-folder", `{"folder_id":"`+testFolderID+`"}`, true); w.Code != 200 {
		t.Fatalf("set folder: %d %s", w.Code, w.Body)
	}
	if body := do(h, "GET", "/", "", false).Body.String(); strings.Contains(body, "no-drive-folder") {
		t.Fatal("notice still shown after setting the folder")
	}
}

func TestFavicon(t *testing.T) {
	_, h := newServer(t)
	w := do(h, "GET", "/static/favicon.svg", "", false)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/svg+xml") || !strings.Contains(w.Body.String(), "<svg") {
		t.Fatalf("favicon.svg: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	w = do(h, "GET", "/favicon.ico", "", false)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/static/favicon.svg" {
		t.Fatalf("favicon.ico: %d %q", w.Code, w.Header().Get("Location"))
	}
	if body := do(h, "GET", "/", "", false).Body.String(); !strings.Contains(body, `<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">`) {
		t.Fatal("layout lacks the icon link")
	}
}

type runsPage struct {
	OK   bool     `json:"ok"`
	Runs []runRow `json:"runs"`
	Next string   `json:"next"`
}

func getRuns(t *testing.T, h http.Handler, query string) runsPage {
	t.Helper()
	w := do(h, "GET", "/api/runs"+query, "", false)
	var p runsPage
	if err := json.Unmarshal(w.Body.Bytes(), &p); w.Code != 200 || err != nil || !p.OK {
		t.Fatalf("GET /api/runs%s = %d %s", query, w.Code, w.Body)
	}
	return p
}

func TestAPIRunsPagination(t *testing.T) {
	s, h := newServer(t)
	if p := getRuns(t, h, ""); len(p.Runs) != 0 || p.Next != "" {
		t.Fatalf("empty history = %+v", p)
	}
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local)
	for i := range 130 {
		id, _ := s.Index.StartRun("schedule", t0.Add(time.Duration(i)*time.Hour))
		s.Index.FinishRun(index.Run{ID: id, Status: index.StatusPartial, Finished: t0.Add(time.Duration(i)*time.Hour + 90*time.Second),
			Uploaded: i, Errors: 1, Bytes: 1536, Message: "<b>x</b>"})
	}

	// Default page size, newest first, labels as on the dashboard.
	p := getRuns(t, h, "")
	if len(p.Runs) != runsPageSize || p.Runs[0].ID != 130 || p.Next != "111" {
		t.Fatalf("first page: %d runs, first %d, next %q", len(p.Runs), p.Runs[0].ID, p.Next)
	}
	r := p.Runs[0]
	if r.Started != "2026-10-06 17:00" || r.TriggerLabel != "ตามตาราง" || r.StatusLabel != "สำเร็จบางส่วน" ||
		r.Size != "1.5 KiB" || r.Duration != "1m30s" || r.Message != "<b>x</b>" || r.Uploaded != 129 {
		t.Fatalf("row = %+v", r)
	}
	// The cursor continues right after the previous page.
	p = getRuns(t, h, "?before="+p.Next+"&limit=5")
	if len(p.Runs) != 5 || p.Runs[0].ID != 110 || p.Next != "106" {
		t.Fatalf("second page: %d runs, first %d, next %q", len(p.Runs), p.Runs[0].ID, p.Next)
	}
	// limit is capped at runsPageMax.
	if p = getRuns(t, h, "?limit=1000"); len(p.Runs) != runsPageMax || p.Next != "31" {
		t.Fatalf("capped page: %d runs, next %q", len(p.Runs), p.Next)
	}
	// The last page ends with an empty cursor, also on an exact fit.
	if p = getRuns(t, h, "?before=31&limit=100"); len(p.Runs) != 30 || p.Next != "" || p.Runs[29].ID != 1 {
		t.Fatalf("last page: %d runs, next %q", len(p.Runs), p.Next)
	}
	if p = getRuns(t, h, "?before=11&limit=10"); len(p.Runs) != 10 || p.Next != "" {
		t.Fatalf("exact last page: %d runs, next %q", len(p.Runs), p.Next)
	}
	for _, bad := range []string{"?before=x", "?before=0", "?before=-3", "?limit=0", "?limit=abc"} {
		if w := do(h, "GET", "/api/runs"+bad, "", false); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}

	// The history page renders the first page and hands the cursor to the JS.
	body := do(h, "GET", "/history", "", false).Body.String()
	if !strings.Contains(body, `data-cursor="111"`) || strings.Count(body, `data-run=`) != runsPageSize {
		t.Fatalf("history page: %d rows, cursor missing", strings.Count(body, `data-run=`))
	}
}

// navLink returns the <a> tag of the nav entry for href.
func navLink(t *testing.T, body, href string) string {
	t.Helper()
	nav := body[strings.Index(body, "<nav>"):strings.Index(body, "</nav>")]
	i := strings.Index(nav, `<a href="`+href+`"`)
	if i < 0 {
		t.Fatalf("nav lacks %s:\n%s", href, nav)
	}
	return nav[i : i+strings.Index(nav[i:], ">")]
}

func TestHistoryTabAndDashboardSummary(t *testing.T) {
	s, h := newServer(t)
	if body := do(h, "GET", "/history", "", false).Body.String(); !strings.Contains(body, "ยังไม่เคยสำรองข้อมูล") ||
		strings.Contains(body, `id="history-rows"`) {
		t.Fatalf("empty history page:\n%s", body)
	}
	if body := do(h, "GET", "/", "", false).Body.String(); !strings.Contains(body, "ยังไม่เคยสำรองข้อมูล") {
		t.Fatalf("empty dashboard summary:\n%s", body)
	}

	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.Local)
	for i := range 3 {
		id, _ := s.Index.StartRun("manual", t0.Add(time.Duration(i)*time.Hour))
		s.Index.FinishRun(index.Run{ID: id, Status: index.StatusSuccess, Finished: t0.Add(time.Duration(i)*time.Hour + 2*time.Minute), Uploaded: 7 + i, Bytes: 2048})
	}
	s.Index.StartRun("schedule", t0.Add(10*time.Hour)) // still running: not the "last finished" run

	hist := do(h, "GET", "/history", "", false).Body.String()
	for _, p := range []string{"/", "/history", "/folders", "/settings", "/logs"} {
		got := navLink(t, hist, p)
		if on := strings.Contains(got, `class="on"`); on != (p == "/history") {
			t.Errorf("nav %s on history page: %s", p, got)
		}
	}
	if !strings.Contains(hist, ">ประวัติ</a>") || strings.Index(hist, `href="/history"`) > strings.Index(hist, `href="/folders"`) ||
		strings.Index(hist, `href="/"`) > strings.Index(hist, `href="/history"`) {
		t.Errorf("tab ประวัติ is not between ภาพรวม and โฟลเดอร์")
	}
	if !strings.Contains(hist, `id="history-rows" data-cursor=""`) || strings.Count(hist, `data-run=`) != 4 {
		t.Errorf("history page rows: %d", strings.Count(hist, `data-run=`))
	}
	// The next-run label attribute must never appear on the history element.
	if strings.Contains(hist, "data-next") {
		t.Error("history page uses data-next, which the live poll overwrites")
	}

	dash := do(h, "GET", "/", "", false).Body.String()
	if got := navLink(t, dash, "/"); !strings.Contains(got, `class="on"`) {
		t.Errorf("dashboard nav: %s", got)
	}
	if strings.Contains(navLink(t, dash, "/history"), `class="on"`) {
		t.Error("history tab highlighted on the dashboard")
	}
	for _, gone := range []string{`id="history"`, `id="history-rows"`, "data-cursor", "<table"} {
		if strings.Contains(dash, gone) {
			t.Errorf("dashboard still has the history table (%s)", gone)
		}
	}
	// Summary of the newest *finished* run (#3), with a link to the history.
	if !strings.Contains(dash, `href="/history">ดูประวัติทั้งหมด →`) || !strings.Contains(dash, "ใหม่ 9 ·") ||
		!strings.Contains(dash, "2.0 KiB · 2m0s") || !strings.Contains(dash, "2026-10-01 10:00") {
		t.Errorf("last-run summary missing:\n%s", dash)
	}
	// data-next is the live next-run label on the dashboard only.
	if strings.Count(dash, "data-next") != 1 {
		t.Errorf("data-next count = %d", strings.Count(dash, "data-next"))
	}
}

func TestHistoryEnd(t *testing.T) {
	s, h := newServer(t)
	id, _ := s.Index.StartRun("manual", time.Now())
	s.Index.FinishRun(index.Run{ID: id, Status: index.StatusSuccess, Finished: time.Now()})
	body := do(h, "GET", "/history", "", false).Body.String()
	if !strings.Contains(body, `data-cursor=""`) || !strings.Contains(body, "แสดงครบทุกรายการแล้ว") {
		t.Fatalf("single page history lacks the end marker:\n%s", body)
	}
}

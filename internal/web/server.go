// Package web serves the dashboard, folder, settings and log pages and a
// small JSON API used by the embedded JavaScript.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"laptop-backup/internal/auth"
	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/index"
	"laptop-backup/internal/schedule"
	bsync "laptop-backup/internal/sync"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	Config   *config.Store
	Auth     *auth.Manager
	Runner   *bsync.Runner
	Sched    *schedule.Scheduler
	Index    *index.DB
	Paths    config.Paths
	Log      *slog.Logger
	NewDrive func(ctx context.Context) (drive.Client, error)
	// Hosts are the accepted Host headers (DNS-rebinding protection).
	Hosts []string

	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"when":     fmtTime,
	"bytes":    fmtBytes,
	"status":   statusLabel,
	"trigger":  triggerLabel,
	"duration": func(d time.Duration) string { return d.String() },
}

// Handler builds the HTTP handler.
func (s *Server) Handler() (http.Handler, error) {
	s.pages = map[string]*template.Template{}
	for _, p := range []string{"dashboard", "history", "folders", "settings", "logs"} {
		t, err := template.New("layout.html").Funcs(funcs).Funcs(template.FuncMap{"tilde": s.tilde}).
			ParseFS(assets, "templates/layout.html", "templates/"+p+".html")
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}
	static, _ := fs.Sub(assets, "static")

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	// Browsers ask for /favicon.ico on their own; send them to the SVG.
	mux.Handle("GET /favicon.ico", http.RedirectHandler("/static/favicon.svg", http.StatusMovedPermanently))
	mux.HandleFunc("GET /{$}", s.page("dashboard"))
	mux.HandleFunc("GET /history", s.page("history"))
	mux.HandleFunc("GET /folders", s.page("folders"))
	mux.HandleFunc("GET /settings", s.page("settings"))
	mux.HandleFunc("GET /logs", s.page("logs"))

	mux.HandleFunc("GET /api/status", s.apiStatus)
	mux.HandleFunc("GET /api/runs", s.apiRuns)
	mux.HandleFunc("POST /api/run", s.apiRun)
	mux.HandleFunc("POST /api/cancel", s.apiCancel)
	mux.HandleFunc("GET /api/browse", s.apiBrowse)
	mux.HandleFunc("GET /api/preview", s.apiPreview)
	mux.HandleFunc("POST /api/folders/add", s.apiFolderAdd)
	mux.HandleFunc("POST /api/folders/remove", s.apiFolderRemove)
	mux.HandleFunc("POST /api/excludes", s.apiExcludes)
	mux.HandleFunc("POST /api/schedule", s.apiSchedule)
	mux.HandleFunc("POST /api/upload", s.apiUpload)
	mux.HandleFunc("POST /api/drive-folder", s.apiDriveFolder)
	mux.HandleFunc("POST /api/drive-folder/test", s.apiDriveTest)
	mux.HandleFunc("POST /api/disconnect", s.apiDisconnect)
	mux.HandleFunc("GET /api/log", s.apiLog)

	mux.HandleFunc("GET /oauth/start", s.oauthStart)
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	return s.guard(mux), nil
}

// guard rejects unknown Host headers (DNS rebinding) and cross-site POSTs:
// state-changing requests must carry the X-Backup-UI header, which a
// foreign page cannot add without a CORS preflight we never approve.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(s.Hosts, r.Host) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Backup-UI") != "1" {
			http.Error(w, "missing X-Backup-UI header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type pageData struct {
	Page     string
	Auth     auth.Status
	Cfg      config.Config
	Run      bsync.Status
	Next     string
	Runs     []index.Run
	RunsNext string     // /api/runs cursor after Runs on the history page ("" = all shown)
	LastRun  *index.Run // newest finished run, for the dashboard summary
	Paths    config.Paths
	Msg, Err string
	// Logs page
	SelRun    int64
	RunErrors []index.RunError
}

func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := pageData{
			Page:  name,
			Auth:  s.Auth.Status(),
			Cfg:   s.Config.Get(),
			Run:   s.Runner.Status(),
			Next:  s.nextLabel(),
			Paths: s.Paths,
			Msg:   r.URL.Query().Get("msg"),
			Err:   r.URL.Query().Get("err"),
		}
		var err error
		switch name {
		case "history":
			// First page of the history; the rest is fetched while scrolling.
			var more bool
			if d.Runs, more, err = s.Index.RunsBefore(0, runsPageSize); err != nil {
				s.Log.Error("recent runs", "err", err)
			}
			d.RunsNext = runsCursor(d.Runs, more)
		case "dashboard":
			if last, err := s.Index.FinishedRuns(1); err != nil {
				s.Log.Error("last run", "err", err)
			} else if len(last) > 0 {
				d.LastRun = &last[0]
			}
		default:
			if d.Runs, err = s.Index.RecentRuns(30); err != nil {
				s.Log.Error("recent runs", "err", err)
			}
		}
		if name == "logs" {
			d.SelRun, _ = strconv.ParseInt(r.URL.Query().Get("run"), 10, 64)
			if d.SelRun == 0 && len(d.Runs) > 0 {
				d.SelRun = d.Runs[0].ID
			}
			if d.RunErrors, err = s.Index.RunErrors(d.SelRun, 500); err != nil {
				s.Log.Error("run errors", "err", err)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := s.pages[name].Execute(w, d); err != nil {
			s.Log.Error("render", "page", name, "err", err)
		}
	}
}

func (s *Server) nextLabel() string {
	next, ok := s.Sched.NextRun()
	if !ok {
		if s.Config.Get().Schedule.Mode == config.ModePaused {
			return "หยุดชั่วคราว"
		}
		return "—"
	}
	if time.Until(next) < time.Minute {
		return "ภายในไม่กี่นาที"
	}
	return fmtTime(next)
}

// --- JSON helpers -----------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func ok(w http.ResponseWriter, extra map[string]any) {
	m := map[string]any{"ok": true}
	for k, v := range extra {
		m[k] = v
	}
	writeJSON(w, http.StatusOK, m)
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// --- API --------------------------------------------------------------

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"run": s.Runner.Status(), "auth": s.Auth.Status(), "next": s.nextLabel()})
}

// Run history page sizes for /api/runs.
const (
	runsPageSize = 20
	runsPageMax  = 100
)

// runRow is one history row with the labels the dashboard template shows,
// so the JavaScript renders exactly what the server does.
type runRow struct {
	ID           int64  `json:"id"`
	Started      string `json:"started"`
	Trigger      string `json:"trigger"`
	TriggerLabel string `json:"trigger_label"`
	Status       string `json:"status"`
	StatusLabel  string `json:"status_label"`
	Message      string `json:"message"`
	Uploaded     int    `json:"uploaded"`
	Updated      int    `json:"updated"`
	Trashed      int    `json:"trashed"`
	Errors       int    `json:"errors"`
	Bytes        int64  `json:"bytes"`
	Size         string `json:"size"`
	Duration     string `json:"duration"`
}

// runsCursor is the "next" value after runs: the last ID, or "" at the end.
func runsCursor(runs []index.Run, more bool) string {
	if !more || len(runs) == 0 {
		return ""
	}
	return strconv.FormatInt(runs[len(runs)-1].ID, 10)
}

// apiRuns pages through the run history, newest first:
// GET /api/runs?before=<run id>&limit=<n>. Pass the returned "next" as
// before to get the following page; "next" is "" after the last one.
func (s *Server) apiRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, errors.New("before must be a run ID"))
			return
		}
		before = n
	}
	limit := runsPageSize
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, errors.New("limit must be a positive number"))
			return
		}
		limit = min(n, runsPageMax)
	}
	runs, more, err := s.Index.RunsBefore(before, limit)
	if err != nil {
		s.Log.Error("run history", "err", err)
		fail(w, http.StatusInternalServerError, err)
		return
	}
	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, runRow{
			ID: run.ID, Started: fmtTime(run.Started),
			Trigger: run.Trigger, TriggerLabel: triggerLabel(run.Trigger),
			Status: run.Status, StatusLabel: statusLabel(run.Status), Message: run.Message,
			Uploaded: run.Uploaded, Updated: run.Updated, Trashed: run.Trashed, Errors: run.Errors,
			Bytes: run.Bytes, Size: fmtBytes(run.Bytes), Duration: run.Duration().String(),
		})
	}
	ok(w, map[string]any{"runs": rows, "next": runsCursor(runs, more)})
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Full bool `json:"full"`
	}
	decode(r, &req)
	if !s.Auth.Status().Connected {
		fail(w, http.StatusConflict, auth.ErrNotConnected)
		return
	}
	cfg := s.Config.Get()
	if cfg.DriveFolderID == "" {
		fail(w, http.StatusConflict, bsync.ErrNoDriveFolder)
		return
	}
	if len(cfg.Sources) == 0 {
		fail(w, http.StatusConflict, bsync.ErrNoSources)
		return
	}
	trigger := "manual"
	if req.Full {
		trigger = "full"
	}
	if err := s.Runner.Start(trigger, req.Full); err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	ok(w, nil)
}

func (s *Server) apiCancel(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"cancelled": s.Runner.Cancel()})
}

func (s *Server) apiBrowse(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = s.Paths.Home
	}
	if !filepath.IsAbs(dir) {
		fail(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}
	dir = filepath.Clean(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	dirs := []string{}
	for _, e := range entries {
		if e.IsDir() { // DirEntry reports symlinks as non-dirs: they are skipped
			dirs = append(dirs, e.Name())
		}
	}
	// Hidden folders last.
	slices.SortStableFunc(dirs, func(a, b string) int {
		ha, hb := strings.HasPrefix(a, "."), strings.HasPrefix(b, ".")
		if ha != hb {
			if ha {
				return 1
			}
			return -1
		}
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
	ok(w, map[string]any{"path": dir, "display": s.tilde(dir), "parent": filepath.Dir(dir), "dirs": dirs})
}

func (s *Server) apiPreview(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Clean(r.URL.Query().Get("path"))
	if !filepath.IsAbs(dir) {
		fail(w, http.StatusBadRequest, errors.New("path must be absolute"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	excludes := s.Config.Get().Excludes
	prefix := dir + "/"
	var files, skipped int
	var size int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if p == dir {
				return err
			}
			skipped++
			return nil
		}
		if p == dir {
			return nil
		}
		if bsync.Excluded(excludes, strings.TrimPrefix(p, prefix), d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				files++
				size += info.Size()
			}
		}
		return nil
	})
	partial := errors.Is(err, context.DeadlineExceeded)
	if err != nil && !partial {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ok(w, map[string]any{"files": files, "bytes": size, "human": fmtBytes(size), "partial": partial, "unreadable": skipped})
}

func (s *Server) apiFolderAdd(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := decode(r, &req); err != nil || !filepath.IsAbs(req.Path) {
		fail(w, http.StatusBadRequest, errors.New("ต้องเป็น path แบบเต็ม"))
		return
	}
	p := filepath.Clean(req.Path)
	if info, err := os.Lstat(p); err != nil || !info.IsDir() {
		fail(w, http.StatusBadRequest, fmt.Errorf("ไม่ใช่โฟลเดอร์ (หรือเป็น symlink): %s", p))
		return
	}
	err := s.Config.Update(func(c *config.Config) {
		if !slices.Contains(c.Sources, p) {
			c.Sources = append(c.Sources, p)
			slices.Sort(c.Sources)
		}
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.Log.Info("source folder added", "path", p)
	ok(w, nil)
}

func (s *Server) apiFolderRemove(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.Config.Update(func(c *config.Config) {
		c.Sources = slices.DeleteFunc(c.Sources, func(x string) bool { return x == req.Path })
	})
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.Log.Info("source folder removed (Drive copy left alone)", "path", req.Path)
	ok(w, nil)
}

func (s *Server) apiExcludes(w http.ResponseWriter, r *http.Request) {
	var req struct{ Patterns string }
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	pats := []string{}
	for _, line := range strings.Split(req.Patterns, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Comments are kept (Excluded skips them) but not validated.
		if !strings.HasPrefix(line, "#") {
			if _, err := path.Match(strings.Trim(line, "/"), ""); err != nil {
				fail(w, http.StatusBadRequest, fmt.Errorf("pattern ไม่ถูกต้อง: %q", line))
				return
			}
		}
		pats = append(pats, line)
	}
	if err := s.Config.Update(func(c *config.Config) { c.Excludes = pats }); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ok(w, nil)
}

func (s *Server) apiSchedule(w http.ResponseWriter, r *http.Request) {
	var sch config.Schedule
	if err := decode(r, &sch); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	cur := s.Config.Get().Schedule
	// Keep the values of the modes not being edited.
	if sch.Mode != config.ModeInterval {
		sch.EveryHours = cur.EveryHours
	}
	if sch.Mode != config.ModeDaily {
		sch.DailyAt = cur.DailyAt
	}
	if err := s.Config.Update(func(c *config.Config) { c.Schedule = sch }); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.Sched.Reset()
	s.Log.Info("schedule changed", "mode", sch.Mode, "every_hours", sch.EveryHours, "daily_at", sch.DailyAt)
	ok(w, nil)
}

func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Workers   int `json:"upload_workers"`
		LimitKBps int `json:"upload_limit_kbps"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	err := s.Config.Update(func(c *config.Config) { c.UploadWorkers, c.UploadLimitKBps = req.Workers, req.LimitKBps })
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	// The worker count is read when a run starts; the limit applies at once.
	s.Runner.SetUploadLimit(req.LimitKBps)
	s.Log.Info("upload settings changed", "workers", req.Workers, "limit_kbps", req.LimitKBps)
	ok(w, nil)
}

func (s *Server) apiDriveFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID string `json:"folder_id"`
	}
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := strings.TrimSpace(req.FolderID)
	if id == "" {
		fail(w, http.StatusBadRequest, errors.New("กรุณาใส่ Folder ID"))
		return
	}
	if s.Runner.Status().Running {
		fail(w, http.StatusConflict, bsync.ErrBusy)
		return
	}
	if err := s.Config.Update(func(c *config.Config) { c.DriveFolderID = id }); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.Log.Info("Drive root folder changed", "id", id)
	ok(w, nil)
}

func (s *Server) apiDriveTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID string `json:"folder_id"`
	}
	if err := decode(r, &req); err != nil || !config.ValidDriveID(strings.TrimSpace(req.FolderID)) {
		fail(w, http.StatusBadRequest, errors.New("Drive folder ID ไม่ถูกต้อง"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, err := s.NewDrive(ctx)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	f, err := c.Get(ctx, strings.TrimSpace(req.FolderID))
	switch {
	case err != nil:
		fail(w, http.StatusBadGateway, fmt.Errorf("เข้าถึงไม่ได้: %w", err))
	case !f.Folder:
		fail(w, http.StatusBadRequest, fmt.Errorf("%q ไม่ใช่โฟลเดอร์", f.Name))
	case f.Trashed:
		fail(w, http.StatusBadRequest, fmt.Errorf("โฟลเดอร์ %q อยู่ในถังขยะ", f.Name))
	case !f.CanAddChildren:
		fail(w, http.StatusForbidden, fmt.Errorf("ไม่มีสิทธิ์เขียนในโฟลเดอร์ %q", f.Name))
	default:
		ok(w, map[string]any{"name": f.Name})
	}
}

func (s *Server) apiDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Disconnect(); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.Log.Info("Google account disconnected")
	ok(w, nil)
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, tail(s.Paths.LogFile(), 256<<10))
}

// tail returns roughly the last limit bytes of a file, starting at a line.
func tail(name string, limit int64) string {
	f, err := os.Open(name)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err.Error()
	}
	off := max(info.Size()-limit, 0)
	buf := make([]byte, info.Size()-off)
	n, _ := f.ReadAt(buf, off)
	buf = buf[:n]
	if off > 0 {
		if i := strings.IndexByte(string(buf), '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return string(buf)
}

// --- OAuth ------------------------------------------------------------

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	// r.Host was checked by guard, so it is safe to send the browser back.
	u, err := s.Auth.AuthURL("http://" + r.Host + "/settings")
	if err != nil {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, u, http.StatusFound)
}

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back := func(to, key, msg string) {
		if to == "" {
			to = "/settings"
		}
		http.Redirect(w, r, to+"?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if e := q.Get("error"); e != "" {
		back("", "err", "Google: "+e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	to, err := s.Auth.Exchange(ctx, q.Get("state"), q.Get("code"))
	if err != nil {
		s.Log.Warn("OAuth exchange failed", "err", err)
		back(to, "err", err.Error())
		return
	}
	s.Log.Info("Google account connected", "email", s.Auth.Status().Email)
	s.Sched.Reset()
	back(to, "msg", "เชื่อมต่อบัญชี Google แล้ว")
}

// --- formatting -------------------------------------------------------

func (s *Server) tilde(p string) string {
	if p == s.Paths.Home {
		return "~"
	}
	if strings.HasPrefix(p, s.Paths.Home+"/") {
		return "~" + p[len(s.Paths.Home):]
	}
	return p
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func statusLabel(s string) string {
	switch s {
	case index.StatusSuccess:
		return "สำเร็จ"
	case index.StatusPartial:
		return "สำเร็จบางส่วน"
	case index.StatusFailed:
		return "ล้มเหลว"
	case index.StatusCancelled:
		return "ยกเลิก"
	case index.StatusInterrupted:
		return "ถูกขัดจังหวะ"
	case index.StatusRunning:
		return "กำลังทำงาน"
	}
	return s
}

func triggerLabel(s string) string {
	switch s {
	case "schedule":
		return "ตามตาราง"
	case "manual":
		return "สั่งเอง"
	case "full":
		return "สแกนใหม่ทั้งหมด"
	}
	return s
}

// LoopbackHosts returns the Host values accepted for a listen address,
// plus proxyHost (the name a local reverse proxy forwards, e.g. "backup")
// when it is not empty.
func LoopbackHosts(addr, proxyHost string) []string {
	_, port, _ := net.SplitHostPort(addr)
	hosts := []string{"127.0.0.1:" + port, "localhost:" + port}
	if proxyHost != "" {
		hosts = append(hosts, proxyHost)
	}
	return hosts
}

package sync

import (
	"context"
	"errors"
	"log/slog"
	stdsync "sync"
	"time"

	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/index"
	"laptop-backup/internal/ratelimit"
)

var (
	ErrBusy      = errors.New("มีการสำรองข้อมูลกำลังทำงานอยู่แล้ว")
	ErrNoSources = errors.New("ยังไม่ได้เลือกโฟลเดอร์ที่จะสำรอง")
	// ErrNoDriveFolder: config.DriveFolderID is still empty.
	ErrNoDriveFolder = errors.New("ยังไม่ได้ตั้งค่าโฟลเดอร์ปลายทางบน Google Drive (ไปที่หน้าตั้งค่า)")
)

// Runner owns the one-run-at-a-time lock, live progress and run history.
type Runner struct {
	Index  *index.DB
	Config *config.Store
	Home   string
	Log    *slog.Logger
	// NewDrive returns a client for the current Google account, or an
	// error when not connected.
	NewDrive func(ctx context.Context) (drive.Client, error)
	Fatal    func(error) bool
	// BaseCtx is the parent of runs started with Start.
	BaseCtx context.Context
	// OnDone is called after every run attempt (e.g. to refresh the
	// scheduler's next-run time).
	OnDone func()

	wg       stdsync.WaitGroup
	mu       stdsync.Mutex
	running  bool
	cancel   context.CancelFunc
	progress *Progress
	current  index.Run
	// limiter is shared by all runs so a new limit reaches a running one.
	limiter ratelimit.Limiter
}

// Status describes the run in progress, if any.
type Status struct {
	Running bool      `json:"running"`
	RunID   int64     `json:"run_id"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
	Stats   Stats     `json:"stats"`
}

func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return Status{}
	}
	return Status{Running: true, RunID: r.current.ID, Trigger: r.current.Trigger, Started: r.current.Started, Stats: r.progress.Snapshot()}
}

func (r *Runner) acquire(ctx context.Context) (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return nil, ErrBusy
	}
	ctx, r.cancel = context.WithCancel(ctx)
	r.running, r.progress, r.current = true, &Progress{}, index.Run{}
	r.wg.Add(1)
	return ctx, nil
}

func (r *Runner) release() {
	r.mu.Lock()
	r.cancel()
	r.running, r.cancel = false, nil
	r.mu.Unlock()
	r.wg.Done()
	if r.OnDone != nil {
		r.OnDone()
	}
}

// SetUploadLimit changes the upload speed limit (KiB/s, 0 = unlimited),
// including for a run in progress.
func (r *Runner) SetUploadLimit(kbps int) { r.limiter.SetRate(int64(kbps) << 10) }

// UploadLimit returns the current limit in bytes per second (0 = unlimited).
func (r *Runner) UploadLimit() int64 { return r.limiter.Rate() }

// Wait blocks until the current run (if any) has finished.
func (r *Runner) Wait() { r.wg.Wait() }

// Cancel stops the current run; it reports whether one was running.
func (r *Runner) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		r.cancel()
	}
	return r.running
}

// Start begins a run in the background (for "Backup now").
func (r *Runner) Start(trigger string, full bool) error {
	ctx, err := r.acquire(r.BaseCtx)
	if err != nil {
		return err
	}
	go func() {
		defer r.release()
		r.run(ctx, trigger, full)
	}()
	return nil
}

// Run performs a run and blocks until it is done (for the scheduler).
func (r *Runner) Run(ctx context.Context, trigger string, full bool) error {
	ctx, err := r.acquire(ctx)
	if err != nil {
		r.Log.Info("scheduled run skipped", "reason", err)
		return err
	}
	defer r.release()
	return r.run(ctx, trigger, full)
}

func (r *Runner) run(ctx context.Context, trigger string, full bool) error {
	cfg := r.Config.Get()
	if len(cfg.Sources) == 0 {
		r.Log.Info("run skipped", "trigger", trigger, "reason", ErrNoSources)
		return ErrNoSources
	}
	if cfg.DriveFolderID == "" {
		r.Log.Info("run skipped", "trigger", trigger, "reason", ErrNoDriveFolder)
		return ErrNoDriveFolder
	}
	client, err := r.NewDrive(ctx)
	if err != nil {
		r.Log.Warn("run skipped", "trigger", trigger, "reason", err)
		return err
	}

	start := time.Now()
	id, err := r.Index.StartRun(trigger, start)
	if err != nil {
		r.Log.Error("cannot record run", "err", err)
		return err
	}
	r.mu.Lock()
	r.current = index.Run{ID: id, Trigger: trigger, Started: start}
	p := r.progress
	r.mu.Unlock()
	r.Log.Info("backup started", "run", id, "trigger", trigger, "full", full,
		"workers", cfg.UploadWorkers, "limit_kbps", cfg.UploadLimitKBps)

	r.SetUploadLimit(cfg.UploadLimitKBps)
	eng := &Engine{Drive: client, Index: r.Index, Home: r.Home, Log: r.Log, Fatal: r.Fatal, Limiter: &r.limiter}
	runErr := eng.Run(ctx, cfg, id, full, p)

	s := p.Snapshot()
	rec := index.Run{ID: id, Trigger: trigger, Started: start, Finished: time.Now(),
		Scanned: s.Scanned, Uploaded: s.Uploaded, Updated: s.Updated, Trashed: s.Trashed, Errors: s.Errors, Bytes: s.Bytes}
	switch {
	case ctx.Err() != nil:
		rec.Status, rec.Message = index.StatusCancelled, "ยกเลิกแล้ว"
	case runErr != nil:
		rec.Status, rec.Message = index.StatusFailed, runErr.Error()
	case s.Errors > 0:
		rec.Status = index.StatusPartial
	default:
		rec.Status = index.StatusSuccess
	}
	if err := r.Index.FinishRun(rec); err != nil {
		r.Log.Error("cannot record run", "err", err)
	}
	if err := r.Index.Prune(200); err != nil {
		r.Log.Warn("prune run history", "err", err)
	}
	r.Log.Info("backup finished", "run", id, "status", rec.Status, "scanned", s.Scanned, "uploaded", s.Uploaded,
		"updated", s.Updated, "trashed", s.Trashed, "errors", s.Errors, "bytes", s.Bytes, "duration", rec.Duration(), "err", runErr)
	return runErr
}

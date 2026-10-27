package sync

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/drive/drivetest"
	"laptop-backup/internal/index"
)

// blockingDrive holds the first Drive call until release is closed.
type blockingDrive struct {
	*drivetest.Fake
	started, release chan struct{}
}

func (b *blockingDrive) FindFolder(ctx context.Context, parent, name string) (string, bool, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	return b.Fake.FindFolder(ctx, parent, name)
}

func TestRunnerOneRunAtATimeAndCancel(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	store, _ := config.Load(filepath.Join(t.TempDir(), "config.json"))
	store.Update(func(c *config.Config) { *c = e.cfg })
	bd := &blockingDrive{Fake: e.drive, started: make(chan struct{}, 1), release: make(chan struct{})}
	r := &Runner{
		Index: e.idx, Config: store, Home: e.home, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewDrive: func(context.Context) (drive.Client, error) { return bd, nil },
		BaseCtx:  context.Background(),
	}

	if err := r.Start("manual", false); err != nil {
		t.Fatal(err)
	}
	<-bd.started
	if err := r.Start("manual", false); err != ErrBusy {
		t.Fatalf("second Start = %v, want ErrBusy", err)
	}
	if err := r.Run(context.Background(), "schedule", false); err != ErrBusy {
		t.Fatalf("Run while busy = %v", err)
	}
	if !r.Status().Running || !r.Cancel() {
		t.Fatal("expected a running run to cancel")
	}
	r.Wait()
	runs, _ := e.idx.RecentRuns(5)
	if len(runs) != 1 || runs[0].Status != index.StatusCancelled {
		t.Fatalf("runs = %+v", runs)
	}

	// Next run completes and is recorded as a success.
	close(bd.release)
	if err := r.Run(context.Background(), "schedule", false); err != nil {
		t.Fatal(err)
	}
	if runs, _ := e.idx.RecentRuns(5); runs[0].Status != index.StatusSuccess || runs[0].Uploaded != 1 {
		t.Fatalf("runs = %+v", runs[0])
	}
}

func TestRunnerAppliesUploadLimit(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.cfg.UploadLimitKBps = 512
	store, _ := config.Load(filepath.Join(t.TempDir(), "config.json"))
	store.Update(func(c *config.Config) { *c = e.cfg })
	r := &Runner{
		Index: e.idx, Config: store, Home: e.home, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewDrive: func(context.Context) (drive.Client, error) { return e.drive, nil },
		BaseCtx:  context.Background(),
	}
	if err := r.Run(context.Background(), "manual", false); err != nil {
		t.Fatal(err)
	}
	if r.UploadLimit() != 512<<10 {
		t.Fatalf("limit = %d", r.UploadLimit())
	}
	r.SetUploadLimit(0)
	if r.UploadLimit() != 0 {
		t.Fatalf("limit = %d", r.UploadLimit())
	}
}

func TestRunnerRefusesWithoutDriveFolder(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.cfg.DriveFolderID = ""
	store, _ := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(c *config.Config) { *c = e.cfg }); err != nil {
		t.Fatal(err)
	}
	r := &Runner{
		Index: e.idx, Config: store, Home: e.home, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewDrive: func(context.Context) (drive.Client, error) { return e.drive, nil },
		BaseCtx:  context.Background(),
	}
	if err := r.Run(context.Background(), "schedule", false); err != ErrNoDriveFolder {
		t.Fatalf("Run = %v, want ErrNoDriveFolder", err)
	}
	if runs, _ := e.idx.RecentRuns(5); len(runs) != 0 || e.drive.Creates != 0 {
		t.Fatalf("runs = %+v, creates = %d", runs, e.drive.Creates)
	}
}

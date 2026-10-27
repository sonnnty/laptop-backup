package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"laptop-backup/internal/drive"
)

func (e *env) writeN(n int) {
	for i := range n {
		e.write(fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("content %d", i))
	}
}

// timedRun uploads 40 new files through a fake with 20 ms per call.
func timedRun(t *testing.T, workers int) (time.Duration, int) {
	e := newEnv(t)
	e.writeN(40)
	e.drive.Latency = 20 * time.Millisecond
	e.cfg.UploadWorkers = workers
	start := time.Now()
	s := e.run(false)
	d := time.Since(start)
	if s.Uploaded != 40 || s.Errors != 0 || e.drive.Count("f39.txt") != 1 {
		t.Fatalf("%d workers: stats = %+v", workers, s)
	}
	return d, e.drive.MaxInFlight
}

func TestUploadsOverlap(t *testing.T) {
	seq, seqMax := timedRun(t, 1)
	par, parMax := timedRun(t, 4)
	t.Logf("40 files × 20 ms: 1 worker %v, 4 workers %v (%.1fx)", seq.Round(time.Millisecond), par.Round(time.Millisecond), float64(seq)/float64(par))
	if seqMax != 1 || parMax != 4 {
		t.Fatalf("max calls in flight: 1 worker %d, 4 workers %d", seqMax, parMax)
	}
	// 40 × 20 ms = 800 ms sequential; 4 workers need ~200 ms plus the
	// folder lookups on the walk.
	if par > seq/2 {
		t.Fatalf("4 workers took %v, 1 worker %v", par, seq)
	}
}

func TestFatalErrorInWorkerAbortsRun(t *testing.T) {
	e := newEnv(t)
	e.writeN(40)
	errRevoked := errors.New("token revoked")
	e.drive.Latency = 10 * time.Millisecond
	e.drive.FailWith["f01.txt"] = drive.ErrOffline // pauses forever
	e.drive.FailWith["f05.txt"] = errRevoked
	done := make(chan struct{})
	var s Stats
	var err error
	go func() {
		defer close(done)
		s, err = e.runErr(false, func(eng *Engine) { eng.Fatal = func(err error) bool { return errors.Is(err, errRevoked) } })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop after a fatal error")
	}
	if !errors.Is(err, errRevoked) {
		t.Fatalf("err = %v", err)
	}
	if e.drive.Creates > 20 || s.Paused || s.Errors != 0 {
		t.Fatalf("run went on after the fatal error: creates=%d stats=%+v", e.drive.Creates, s)
	}
}

func TestCancelStopsPausedWorkers(t *testing.T) {
	e := newEnv(t)
	e.writeN(20)
	e.drive.FailWith["f03.txt"] = drive.ErrOffline // its worker waits for the network
	ctx, cancel := context.WithCancel(context.Background())
	e.ctx = ctx
	time.AfterFunc(100*time.Millisecond, cancel)
	if s, err := e.runErr(false, nil); !errors.Is(err, context.Canceled) || s.Paused {
		t.Fatalf("err = %v, stats = %+v", err, s)
	}
}

// TestSweepWaitsForWorkers changes every file (each needs an upload on a
// worker) and deletes one: the sweep must not run before the changed
// files are marked seen, or it would trash them.
func TestSweepWaitsForWorkers(t *testing.T) {
	e := newEnv(t)
	e.writeN(30)
	e.write("dir/a.txt", "a")
	e.run(false)

	for i := range 30 {
		e.write(fmt.Sprintf("f%02d.txt", i), fmt.Sprintf("changed content %d", i))
	}
	os.Remove(filepath.Join(e.src, "f07.txt"))
	os.RemoveAll(filepath.Join(e.src, "dir"))
	e.drive.Latency = 10 * time.Millisecond
	s := e.run(false)
	if s.Updated != 29 || s.Trashed != 2 || e.drive.Trashes != 2 || s.Errors != 0 {
		t.Fatalf("stats = %+v, trash calls = %d", s, e.drive.Trashes)
	}
	for i := range 30 {
		got, ok := e.remote(fmt.Sprintf("f%02d.txt", i))
		if ok != (i != 7) || ok && got != fmt.Sprintf("changed content %d", i) {
			t.Fatalf("f%02d.txt = %q, %v", i, got, ok)
		}
	}
	if n, _ := e.idx.Count(); n != 30 { // source folder + 29 files
		t.Fatalf("index has %d entries", n)
	}
}

func TestPausedWhileAnyWorkerIsPaused(t *testing.T) {
	var p Progress
	if !p.pause(1) || !p.Snapshot().Paused {
		t.Fatal("first pause")
	}
	if p.pause(1) || p.pause(-1) || !p.Snapshot().Paused {
		t.Fatal("one worker resumed, another still paused: run must stay paused")
	}
	if !p.pause(-1) || p.Snapshot().Paused {
		t.Fatal("last worker resumed")
	}

	e := newEnv(t)
	e.writeN(6)
	e.drive.Offline = true
	for i := range 6 {
		e.drive.FailUploads[fmt.Sprintf("f%02d.txt", i)] = 8 // past the 5 normal tries
	}
	if s := e.run(false); s.Uploaded != 6 || s.Errors != 0 || s.Paused {
		t.Fatalf("stats = %+v", s)
	}
}

func TestFailedTrashKeepsIndexRow(t *testing.T) {
	refused := errors.New("insufficient permissions")

	// A file replaced by a folder.
	e := newEnv(t)
	e.write("x", "file")
	e.run(false)
	old, _, _ := e.drive.Lookup(rootID, "Documents/Personal/x")
	os.Remove(filepath.Join(e.src, "x"))
	e.write("x/inner.txt", "i")
	e.drive.FailWith["x"] = refused
	if s := e.run(false); s.Errors != 1 || s.Trashed != 0 || s.Uploaded != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if ent, ok, _ := e.idx.Get(filepath.Join(e.src, "x")); !ok || ent.IsDir || ent.DriveID != old.ID {
		t.Fatalf("index row for x = %+v, %v", ent, ok)
	}
	delete(e.drive.FailWith, "x")
	if s := e.run(false); s.Trashed != 1 || s.Uploaded != 1 {
		t.Fatalf("retry: stats = %+v", s)
	}
	if !e.drive.IsTrashed(old.ID) {
		t.Fatal("old file not trashed on retry")
	}

	// A folder replaced by a file.
	e = newEnv(t)
	e.write("y/a.txt", "a")
	e.run(false)
	dir, _, _ := e.drive.Lookup(rootID, "Documents/Personal/y")
	os.RemoveAll(filepath.Join(e.src, "y"))
	e.write("y", "now a file")
	e.drive.FailWith["y"] = refused
	if s := e.run(false); s.Errors != 1 || s.Trashed != 0 || s.Uploaded != 0 || e.drive.Trashes != 0 {
		t.Fatalf("stats = %+v, trash calls = %d", s, e.drive.Trashes)
	}
	if ent, ok, _ := e.idx.Get(filepath.Join(e.src, "y", "a.txt")); !ok || ent.IsDir {
		t.Fatal("contents of the old folder dropped from the index")
	}
	delete(e.drive.FailWith, "y")
	if s := e.run(false); s.Trashed != 1 || s.Uploaded != 1 || e.drive.Trashes != 1 {
		t.Fatalf("retry: stats = %+v", s)
	}
	if !e.drive.IsTrashed(dir.ID) {
		t.Fatal("old folder not trashed on retry")
	}

	// A deleted file whose trash fails is retried by the next sweep.
	e = newEnv(t)
	e.write("z.txt", "z")
	e.run(false)
	os.Remove(filepath.Join(e.src, "z.txt"))
	e.drive.FailWith["z.txt"] = refused
	if s := e.run(false); s.Errors != 1 || s.Trashed != 0 {
		t.Fatalf("stats = %+v", s)
	}
	delete(e.drive.FailWith, "z.txt")
	if s := e.run(false); s.Trashed != 1 {
		t.Fatalf("retry: stats = %+v", s)
	}
}

// Package sync mirrors local folders into a Drive folder: new files are
// uploaded, changed files are updated in place (same Drive ID), and files
// deleted locally are moved to the Drive trash.
package sync

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"time"

	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/index"
	"laptop-backup/internal/ratelimit"
)

// Stats is a live snapshot of a run.
type Stats struct {
	Phase    string `json:"phase"`
	Scanned  int    `json:"scanned"`
	Uploaded int    `json:"uploaded"`
	Updated  int    `json:"updated"`
	Trashed  int    `json:"trashed"`
	Errors   int    `json:"errors"`
	Bytes    int64  `json:"bytes"`
	Current  string `json:"current"`
	// Paused is set while the run waits for the network to come back.
	Paused bool `json:"paused"`
}

// Progress is updated by the engine and read by the web UI.
type Progress struct {
	mu     stdsync.Mutex
	s      Stats
	paused int // workers waiting for the network
}

func (p *Progress) update(fn func(*Stats)) {
	p.mu.Lock()
	fn(&p.s)
	p.mu.Unlock()
}

// pause adds delta to the number of workers waiting for the network and
// reports whether that switched the run between paused and running.
func (p *Progress) pause(delta int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	was := p.paused > 0
	p.paused += delta
	p.s.Paused = p.paused > 0
	return was != p.s.Paused
}

func (p *Progress) Snapshot() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.s
}

// Engine performs one mirror run.
type Engine struct {
	Drive drive.Client
	Index *index.DB
	Home  string
	Log   *slog.Logger
	// Fatal reports errors that must abort the run (e.g. a revoked token).
	Fatal func(error) bool
	// Retry policy for transient Drive errors (defaults: 5 tries, 1s base).
	Retries   int
	RetryBase time.Duration
	// OfflineWait is how often a run paused by a lost network tries again
	// (default 15s).
	OfflineWait time.Duration
	// Limiter caps the combined upload speed (nil = unlimited).
	Limiter *ratelimit.Limiter
}

// fatalError aborts the whole run (index failure, revoked auth, cancel).
type fatalError struct{ error }

func (e fatalError) Unwrap() error { return e.error }

var errSkipSource = errors.New("source skipped")

type run struct {
	*Engine
	// ctx is cancelled, with the error as its cause, by the first fatal
	// error in the walk or in any worker.
	ctx      context.Context
	cancel   context.CancelCauseFunc
	id       int64
	root     string
	excludes []string
	p        *Progress

	jobs    chan func() error
	pending stdsync.WaitGroup // jobs queued or running
	workers stdsync.WaitGroup
}

// Run mirrors cfg.Sources. With full set the index is rebuilt from Drive
// first. Per-file problems are recorded against runID and do not stop the
// run; the returned error is fatal (cancelled, auth revoked, index broken).
//
// The walk runs on the calling goroutine and creates Drive folders itself,
// so a folder always exists (once) before its files are queued. Files that
// need Drive are handed to cfg.UploadWorkers workers through a small
// bounded queue; unchanged files never leave the walk.
func (e *Engine) Run(ctx context.Context, cfg config.Config, runID int64, full bool, p *Progress) error {
	if e.Retries <= 0 {
		e.Retries = 5
	}
	if e.RetryBase <= 0 {
		e.RetryBase = time.Second
	}
	if e.OfflineWait <= 0 {
		e.OfflineWait = 15 * time.Second
	}
	workers := cfg.UploadWorkers
	if workers < 1 {
		workers = config.DefaultUploadWorkers
	}
	workers = min(workers, config.MaxUploadWorkers)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r := &run{Engine: e, ctx: ctx, cancel: cancel, id: runID, root: cfg.DriveFolderID, excludes: cfg.Excludes, p: p,
		jobs: make(chan func() error, workers)}

	// The index is only valid for the Drive root it was built against.
	prev, err := e.Index.Meta("drive_root")
	if err != nil {
		return err
	}
	if full || prev != r.root {
		if prev != "" && prev != r.root {
			e.Log.Info("Drive root folder changed; resetting index", "old", prev, "new", r.root)
		}
		if err := e.Index.Clear(); err != nil {
			return err
		}
		if err := e.Index.SetMeta("drive_root", r.root); err != nil {
			return err
		}
	}

	for range workers {
		r.workers.Add(1)
		go r.worker()
	}
	defer func() {
		close(r.jobs)
		r.workers.Wait()
	}()
	for _, src := range NormalizeSources(cfg.Sources) {
		if err := r.source(src, full); err != nil {
			return err
		}
	}
	return nil
}

func (r *run) worker() {
	defer r.workers.Done()
	for job := range r.jobs {
		// After an abort the queue is drained without doing the work.
		if r.ctx.Err() == nil {
			if err := job(); err != nil {
				r.cancel(err) // first error wins; it stops the walk and the other workers
			}
		}
		r.pending.Done()
	}
}

// submit queues job for a worker, blocking while the queue is full. job
// returns only fatal errors, like the walk callbacks.
func (r *run) submit(job func() error) error {
	r.pending.Add(1)
	select {
	case r.jobs <- job:
		return nil
	case <-r.ctx.Done():
		r.pending.Done()
		return r.ctx.Err()
	}
}

// wait blocks until every queued job has finished and returns the error
// that aborted the run, if any.
func (r *run) wait() error {
	r.pending.Wait()
	if r.ctx.Err() != nil {
		return context.Cause(r.ctx)
	}
	return nil
}

// check turns err into a fatal error to propagate, or records it against
// path and returns nil so the run can continue. It is called from the walk
// and from workers: the logger, the index and Progress are all safe for
// concurrent use.
func (r *run) check(path string, err error) error {
	if err == nil {
		return nil
	}
	var fe fatalError
	switch {
	case r.ctx.Err() != nil:
		return r.ctx.Err()
	case errors.As(err, &fe):
		return err
	case r.Fatal != nil && r.Fatal(err):
		return fatalError{err}
	}
	r.Log.Warn("backup error", "path", path, "err", err)
	if e := r.Index.AddRunError(r.id, path, err.Error()); e != nil {
		return fatalError{e}
	}
	r.p.update(func(s *Stats) { s.Errors++ })
	return nil
}

func fatal(err error) error {
	if err == nil {
		return nil
	}
	return fatalError{err}
}

// retry calls fn until it succeeds, fails permanently or tries run out,
// backing off exponentially with jitter. When the tries run out because
// the network is gone (laptop asleep, Wi-Fi switch), the run pauses and
// keeps trying every OfflineWait instead of skipping the file. With several
// workers the run counts as paused while any of them is.
func (r *run) retry(fn func() error) error {
	delay := r.RetryBase
	paused := false
	defer func() {
		if paused && r.p.pause(-1) && r.ctx.Err() == nil {
			r.Log.Info("network is back, resuming")
		}
	}()
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !drive.Retryable(err) || (r.Fatal != nil && r.Fatal(err)) {
			return err
		}
		wait := delay + rand.N(delay/2+1)
		switch {
		case attempt < r.Retries:
			r.Log.Debug("retrying Drive call", "attempt", attempt, "err", err)
			delay *= 2
		case !drive.IsNetwork(err):
			return err
		default:
			if !paused {
				paused = true
				if r.p.pause(1) {
					r.Log.Warn("network unreachable, pausing until it is back", "err", err)
				}
			}
			wait = r.OfflineWait
		}
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (r *run) source(src string, full bool) error {
	r.p.update(func(s *Stats) { s.Current = src })
	info, err := os.Lstat(src)
	if err == nil && !info.IsDir() {
		err = errors.New("not a directory")
	}
	if err != nil {
		// Never sweep a missing source: an unmounted disk must not trash
		// the backup.
		return r.check(src, fmt.Errorf("source folder unavailable, skipped (Drive copy untouched): %w", err))
	}
	if err := r.prepareSource(src, full); err != nil {
		return r.check(src, err)
	}

	prefix := src + "/"
	if src == "/" {
		prefix = "/"
	}
	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if e := r.ctx.Err(); e != nil {
			return e
		}
		if err != nil {
			if path == src {
				if e := r.check(src, err); e != nil {
					return e
				}
				return errSkipSource
			}
			return r.unreadable(path, d, err)
		}
		if path == src {
			return nil
		}
		isDir := d.IsDir()
		if Excluded(r.excludes, strings.TrimPrefix(path, prefix), isDir) {
			if isDir {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case isDir:
			return r.dir(path)
		case d.Type().IsRegular():
			return r.file(path, d)
		default:
			r.Log.Info("skipped non-regular file", "path", path, "type", d.Type().String())
			return nil
		}
	})
	if walkErr != nil && !errors.Is(walkErr, errSkipSource) {
		r.cancel(walkErr)
	}
	// The sweep relies on every live file having been marked seen, so all
	// uploads of this source must be done first.
	if err := r.wait(); err != nil {
		return err
	}
	if walkErr != nil { // errSkipSource
		return nil
	}
	return r.sweep(src)
}

// prepareSource resolves (creating if needed) the Drive folder for src and
// makes sure the index for src matches it. When the remote folder already
// exists but the index does not know it (new source, full rescan, folder
// replaced on Drive) the index is rebuilt from Drive so nothing is uploaded
// twice.
func (r *run) prepareSource(src string, full bool) error {
	parent, id, existed := r.root, r.root, true
	for _, name := range RemotePath(r.Home, src) {
		var err error
		existed = true
		err = r.retry(func() (err error) {
			id, existed, err = r.Drive.FindFolder(r.ctx, parent, name)
			return err
		})
		if err == nil && !existed {
			err = r.retry(func() (err error) {
				id, err = r.Drive.CreateFolder(r.ctx, parent, name)
				return err
			})
		}
		if err != nil {
			return fmt.Errorf("Drive folder %q: %w", name, err)
		}
		parent = id
	}
	ent, ok, err := r.Index.Get(src)
	if err != nil {
		return fatal(err)
	}
	if full || !ok || !ent.IsDir || ent.DriveID != id {
		if err := r.Index.DeleteSubtree(src); err != nil {
			return fatal(err)
		}
		if existed {
			r.p.update(func(s *Stats) { s.Phase = "rescan" })
			if err := r.rebuild(src, id); err != nil {
				return err
			}
		}
	}
	r.p.update(func(s *Stats) { s.Phase = "sync" })
	return fatal(r.Index.Put(index.Entry{Path: src, DriveID: id, IsDir: true}, r.id))
}

// rebuild indexes the existing Drive tree under folderID as local dir.
func (r *run) rebuild(dir, folderID string) error {
	var names map[string]bool
	var subdirs []index.Entry
	err := r.retry(func() error {
		names, subdirs = map[string]bool{}, nil
		return r.Drive.List(r.ctx, folderID, func(f drive.File) error {
			// Google Docs and the like have no md5 and no local
			// counterpart; leaving them out of the index keeps them safe.
			if names[f.Name] || strings.Contains(f.Name, "/") || f.Name == "." || f.Name == ".." || (!f.Folder && f.MD5 == "") {
				r.Log.Info("rescan: ignoring Drive item", "dir", dir, "name", f.Name, "id", f.ID)
				return nil
			}
			names[f.Name] = true
			e := index.Entry{Path: filepath.Join(dir, f.Name), DriveID: f.ID, IsDir: f.Folder, Size: f.Size, MD5: f.MD5}
			if !f.ModTime.IsZero() {
				e.MTime = f.ModTime.UnixNano()
			}
			if f.Folder {
				subdirs = append(subdirs, e)
			}
			return fatal(r.Index.Put(e, 0))
		})
	})
	if err != nil {
		return fmt.Errorf("rescan %s: %w", dir, err)
	}
	for _, sd := range subdirs {
		if err := r.rebuild(sd.Path, sd.DriveID); err != nil {
			return err
		}
	}
	return nil
}

// unreadable handles a walk error below the source root: the item and
// everything under it keep their Drive copies.
func (r *run) unreadable(path string, d fs.DirEntry, err error) error {
	if e := r.check(path, err); e != nil {
		return e
	}
	isDir := d != nil && d.IsDir()
	if e := r.keep(path, isDir); e != nil {
		return e
	}
	if isDir {
		return fs.SkipDir
	}
	return nil
}

// keep protects the index entry at path (and, for a folder, everything
// below it) from this run's sweep.
func (r *run) keep(path string, dir bool) error {
	if err := r.Index.MarkSeen(path, r.id); err != nil {
		return fatal(err)
	}
	if dir {
		return fatal(r.Index.MarkSubtreeSeen(path, r.id))
	}
	return nil
}

// parentID returns the Drive ID of path's parent folder.
func (r *run) parentID(path string) (string, error) {
	ent, ok, err := r.Index.Get(filepath.Dir(path))
	if err != nil {
		return "", fatal(err)
	}
	if !ok || !ent.IsDir {
		return "", errors.New("parent folder is not on Drive")
	}
	return ent.DriveID, nil
}

func (r *run) dir(path string) error {
	ent, ok, err := r.Index.Get(path)
	if err != nil {
		return fatal(err)
	}
	if ok && ent.IsDir {
		return fatal(r.Index.MarkSeen(path, r.id))
	}
	if ok { // was a file, now a folder
		// Done here rather than on a worker: the folder's files need the
		// new folder, and the folder needs the old file's index row.
		trashed, err := r.trash(ent)
		if err != nil {
			return err
		}
		if !trashed {
			// The old file keeps its row so the next run tries again;
			// the folder waits until then.
			if err := r.keep(path, false); err != nil {
				return err
			}
			return fs.SkipDir
		}
	}
	parent, err := r.parentID(path)
	var id string
	if err == nil {
		name := filepath.Base(path)
		err = r.retry(func() error {
			var found bool
			var err error
			if id, found, err = r.Drive.FindFolder(r.ctx, parent, name); err == nil && !found {
				id, err = r.Drive.CreateFolder(r.ctx, parent, name)
			}
			return err
		})
	}
	if err != nil {
		return r.unreadable(path, dirEntry{}, err)
	}
	return fatal(r.Index.Put(index.Entry{Path: path, DriveID: id, IsDir: true}, r.id))
}

// dirEntry is a minimal fs.DirEntry reporting a directory.
type dirEntry struct{ fs.DirEntry }

func (dirEntry) IsDir() bool { return true }

// sameMTime treats times equal at millisecond precision as equal: Drive
// stores modifiedTime in milliseconds, so entries rebuilt from Drive lose
// the sub-millisecond part of the local mtime we uploaded.
func sameMTime(a, b int64) bool { return a/1e6 == b/1e6 }

func (r *run) file(path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return r.unreadable(path, d, err)
	}
	size, mtime := info.Size(), info.ModTime()
	r.p.update(func(s *Stats) { s.Scanned++; s.Current = path })

	ent, ok, err := r.Index.Get(path)
	if err != nil {
		return fatal(err)
	}
	if ok && !ent.IsDir && ent.Size == size && sameMTime(ent.MTime, mtime.UnixNano()) {
		if ent.MTime == mtime.UnixNano() {
			return fatal(r.Index.MarkSeen(path, r.id))
		}
		ent.MTime = mtime.UnixNano()
		return fatal(r.Index.Put(ent, r.id))
	}
	// Everything else talks to Drive: hand it to a worker.
	return r.submit(func() error { return r.syncFile(path, d, ent, ok, size, mtime) })
}

// syncFile is the Drive side of file, run on a worker: it replaces a
// folder that became a file, confirms an mtime-only change by md5, or
// uploads. Like the walk callbacks it returns only fatal errors.
func (r *run) syncFile(path string, d fs.DirEntry, ent index.Entry, ok bool, size int64, mtime time.Time) error {
	if ok && ent.IsDir { // was a folder, now a file
		trashed, err := r.trash(ent)
		if err != nil {
			return err
		}
		if !trashed {
			// Keep the folder's rows so the next run tries again.
			return r.keep(path, true)
		}
		ok = false
	}

	if ok && ent.Size == size {
		// Only mtime changed: compare content with Drive's md5Checksum
		// before uploading.
		sum, err := md5File(path)
		if err != nil {
			return r.unreadable(path, d, err)
		}
		var remote drive.File
		err = r.retry(func() (err error) { remote, err = r.Drive.Get(r.ctx, ent.DriveID); return err })
		switch {
		case err == nil && !remote.Trashed && remote.MD5 == sum:
			ent.MTime, ent.MD5 = mtime.UnixNano(), sum
			return fatal(r.Index.Put(ent, r.id))
		case errors.Is(err, drive.ErrNotFound) || (err == nil && remote.Trashed):
			ok = false // the Drive copy is gone; upload a new one
		case err != nil:
			return r.unreadable(path, d, err)
		}
	}

	f, updated, err := r.push(path, ent.DriveID, ok, mtime)
	if err != nil {
		return r.unreadable(path, d, err)
	}
	if err := r.Index.Put(index.Entry{Path: path, DriveID: f.ID, Size: size, MTime: mtime.UnixNano(), MD5: f.MD5}, r.id); err != nil {
		return fatal(err)
	}
	r.p.update(func(s *Stats) {
		if updated {
			s.Updated++
		} else {
			s.Uploaded++
		}
		s.Bytes += size
	})
	r.Log.Debug("uploaded", "path", path, "updated", updated, "size", size)
	return nil
}

// push updates the existing Drive file in place when there is one, and
// falls back to creating a new file if it was deleted or trashed on Drive.
func (r *run) push(path, driveID string, exists bool, mtime time.Time) (drive.File, bool, error) {
	if exists {
		f, err := r.upload(path, func(rd io.Reader) (drive.File, error) {
			return r.Drive.Update(r.ctx, driveID, rd, mtime)
		})
		if err == nil && !f.Trashed {
			return f, true, nil
		}
		if err != nil && !errors.Is(err, drive.ErrNotFound) {
			return f, true, err
		}
	}
	parent, err := r.parentID(path)
	if err != nil {
		return drive.File{}, false, err
	}
	name := filepath.Base(path)
	f, err := r.upload(path, func(rd io.Reader) (drive.File, error) {
		return r.Drive.Create(r.ctx, parent, name, rd, mtime)
	})
	return f, false, err
}

// upload streams the file to call, reopening it for every retry.
func (r *run) upload(path string, call func(io.Reader) (drive.File, error)) (drive.File, error) {
	var f drive.File
	err := r.retry(func() error {
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		f, err = call(r.Limiter.Reader(r.ctx, fh))
		return err
	})
	return f, err
}

func md5File(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := md5.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// trash moves ent to the Drive trash and forgets it (and, for a folder,
// everything below it) in the index. If Drive refuses, the error is
// recorded, the index rows are left alone so a later run tries again, and
// trashed is false.
func (r *run) trash(ent index.Entry) (trashed bool, err error) {
	err = r.retry(func() error { return r.Drive.Trash(r.ctx, ent.DriveID) })
	if err != nil && !errors.Is(err, drive.ErrNotFound) {
		return false, r.check(ent.Path, fmt.Errorf("move to trash: %w", err))
	}
	if ent.IsDir {
		if err := r.Index.DeleteSubtree(ent.Path); err != nil {
			return false, fatal(err)
		}
	}
	if err := r.Index.Delete(ent.Path); err != nil {
		return false, fatal(err)
	}
	r.p.update(func(s *Stats) { s.Trashed++ })
	r.Log.Info("moved to Drive trash", "path", ent.Path)
	return true, nil
}

// sweep trashes everything below src that was not seen during the walk,
// using the workers. A folder and its contents are never trashed at the
// same time: the contents go with the folder (or, if that fails, wait for
// the next run with it).
func (r *run) sweep(src string) error {
	gone, err := r.Index.Unseen(src, r.id)
	if err != nil {
		return fatal(err)
	}
	r.p.update(func(s *Stats) { s.Phase = "trash" })
	trashedDirs := map[string]bool{}
	for _, ent := range gone {
		if underAny(ent.Path, src, trashedDirs) {
			continue // trashed with its folder
		}
		if ent.IsDir {
			trashedDirs[ent.Path] = true
		}
		if err := r.submit(func() error { _, err := r.trash(ent); return err }); err != nil {
			break // aborted; wait returns why
		}
	}
	if err := r.wait(); err != nil {
		return err
	}
	r.p.update(func(s *Stats) { s.Phase = "sync" })
	return nil
}

func underAny(path, src string, dirs map[string]bool) bool {
	for d := filepath.Dir(path); d != src && len(d) > len(src); d = filepath.Dir(d) {
		if dirs[d] {
			return true
		}
	}
	return false
}

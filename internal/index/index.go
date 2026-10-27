// Package index stores the local-path → Drive-ID mapping and run history in
// SQLite (pure-Go driver, no cgo).
package index

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS files (
	path     TEXT PRIMARY KEY,
	drive_id TEXT NOT NULL,
	is_dir   INTEGER NOT NULL,
	size     INTEGER NOT NULL DEFAULT 0,
	mtime    INTEGER NOT NULL DEFAULT 0,
	md5      TEXT NOT NULL DEFAULT '',
	seen     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	trigger  TEXT NOT NULL,
	status   TEXT NOT NULL,
	started  INTEGER NOT NULL,
	finished INTEGER NOT NULL DEFAULT 0,
	scanned  INTEGER NOT NULL DEFAULT 0,
	uploaded INTEGER NOT NULL DEFAULT 0,
	updated  INTEGER NOT NULL DEFAULT 0,
	trashed  INTEGER NOT NULL DEFAULT 0,
	errors   INTEGER NOT NULL DEFAULT 0,
	bytes    INTEGER NOT NULL DEFAULT 0,
	message  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS run_errors (
	run_id INTEGER NOT NULL,
	at     INTEGER NOT NULL,
	path   TEXT NOT NULL,
	error  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS run_errors_run ON run_errors(run_id);
`

// Run statuses.
const (
	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusPartial     = "partial" // finished, some files failed
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted" // service stopped mid-run
)

type DB struct{ db *sql.DB }

// Entry is one mirrored file or folder.
type Entry struct {
	Path    string // absolute local path
	DriveID string
	IsDir   bool
	Size    int64
	MTime   int64 // local mtime, unix nanoseconds
	MD5     string
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// One connection: no lock contention, and a small, fixed page cache.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init %s: %w", path, err)
	}
	// Runs left "running" belong to a previous process that died.
	if _, err := db.Exec(`UPDATE runs SET status=?, finished=started WHERE status=?`, StatusInterrupted, StatusRunning); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

// subtree returns the [lo, hi) key range of paths strictly below dir:
// every such path starts with dir+"/", and '0' is the byte after '/'.
func subtree(dir string) (lo, hi string) {
	if dir == "/" {
		return "/", "0"
	}
	return dir + "/", dir + "0"
}

func (d *DB) Get(path string) (Entry, bool, error) {
	e := Entry{Path: path}
	err := d.db.QueryRow(`SELECT drive_id, is_dir, size, mtime, md5 FROM files WHERE path=?`, path).
		Scan(&e.DriveID, &e.IsDir, &e.Size, &e.MTime, &e.MD5)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	return e, err == nil, err
}

// Put inserts or replaces e and marks it seen in run.
func (d *DB) Put(e Entry, run int64) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO files(path, drive_id, is_dir, size, mtime, md5, seen) VALUES(?,?,?,?,?,?,?)`,
		e.Path, e.DriveID, e.IsDir, e.Size, e.MTime, e.MD5, run)
	return err
}

func (d *DB) MarkSeen(path string, run int64) error {
	_, err := d.db.Exec(`UPDATE files SET seen=? WHERE path=?`, run, path)
	return err
}

// MarkSubtreeSeen protects everything below dir from the deletion sweep
// (used when dir could not be read).
func (d *DB) MarkSubtreeSeen(dir string, run int64) error {
	lo, hi := subtree(dir)
	_, err := d.db.Exec(`UPDATE files SET seen=? WHERE path>=? AND path<?`, run, lo, hi)
	return err
}

// Unseen lists entries below dir that were not seen in run, sorted by path.
func (d *DB) Unseen(dir string, run int64) ([]Entry, error) {
	lo, hi := subtree(dir)
	rows, err := d.db.Query(`SELECT path, drive_id, is_dir, size, mtime, md5 FROM files
		WHERE path>=? AND path<? AND seen<>? ORDER BY path`, lo, hi, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Path, &e.DriveID, &e.IsDir, &e.Size, &e.MTime, &e.MD5); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) Delete(path string) error {
	_, err := d.db.Exec(`DELETE FROM files WHERE path=?`, path)
	return err
}

// DeleteSubtree removes everything strictly below dir.
func (d *DB) DeleteSubtree(dir string) error {
	lo, hi := subtree(dir)
	_, err := d.db.Exec(`DELETE FROM files WHERE path>=? AND path<?`, lo, hi)
	return err
}

// Clear forgets every file mapping.
func (d *DB) Clear() error {
	_, err := d.db.Exec(`DELETE FROM files`)
	return err
}

// Count returns the number of indexed entries.
func (d *DB) Count() (n int, err error) {
	err = d.db.QueryRow(`SELECT count(*) FROM files`).Scan(&n)
	return n, err
}

func (d *DB) Meta(key string) (string, error) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (d *DB) SetMeta(key, value string) error {
	_, err := d.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?,?)`, key, value)
	return err
}

// Run is one backup run.
type Run struct {
	ID       int64
	Trigger  string
	Status   string
	Started  time.Time
	Finished time.Time
	Scanned  int
	Uploaded int
	Updated  int
	Trashed  int
	Errors   int
	Bytes    int64
	Message  string
}

func (r Run) Duration() time.Duration {
	if r.Finished.IsZero() {
		return 0
	}
	return r.Finished.Sub(r.Started).Round(time.Second)
}

func (d *DB) StartRun(trigger string, at time.Time) (int64, error) {
	res, err := d.db.Exec(`INSERT INTO runs(trigger, status, started) VALUES(?,?,?)`, trigger, StatusRunning, at.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) FinishRun(r Run) error {
	_, err := d.db.Exec(`UPDATE runs SET status=?, finished=?, scanned=?, uploaded=?, updated=?, trashed=?, errors=?, bytes=?, message=? WHERE id=?`,
		r.Status, r.Finished.UnixNano(), r.Scanned, r.Uploaded, r.Updated, r.Trashed, r.Errors, r.Bytes, r.Message, r.ID)
	return err
}

func (d *DB) RecentRuns(n int) ([]Run, error) {
	return d.queryRuns(`SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, n)
}

// RunsBefore returns up to limit runs with an ID below before (0 = from the
// newest), newest first, and whether older runs remain. Keyset pagination
// on the primary key: each page is an index range scan however deep the
// history, and rows added meanwhile do not shift later pages.
func (d *DB) RunsBefore(before int64, limit int) (runs []Run, more bool, err error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	runs, err = d.queryRuns(`SELECT `+runCols+` FROM runs WHERE id < ? ORDER BY id DESC LIMIT ?`, before, limit+1)
	if len(runs) > limit {
		runs, more = runs[:limit], true
	}
	return runs, more, err
}

const runCols = `id, trigger, status, started, finished, scanned, uploaded, updated, trashed, errors, bytes, message`

func (d *DB) queryRuns(query string, args ...any) ([]Run, error) {
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started, finished int64
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Status, &started, &finished, &r.Scanned, &r.Uploaded,
			&r.Updated, &r.Trashed, &r.Errors, &r.Bytes, &r.Message); err != nil {
			return nil, err
		}
		r.Started = time.Unix(0, started)
		if finished != 0 {
			r.Finished = time.Unix(0, finished)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRuns returns the start time of the latest run and of the latest
// completed (success or partial) run.
func (d *DB) LastRuns() (lastStart, lastSuccess time.Time, err error) {
	var a, b sql.NullInt64
	err = d.db.QueryRow(`SELECT max(started), max(CASE WHEN status IN (?,?) THEN started END) FROM runs`,
		StatusSuccess, StatusPartial).Scan(&a, &b)
	if a.Valid {
		lastStart = time.Unix(0, a.Int64)
	}
	if b.Valid {
		lastSuccess = time.Unix(0, b.Int64)
	}
	return lastStart, lastSuccess, err
}

type RunError struct {
	At    time.Time
	Path  string
	Error string
}

func (d *DB) AddRunError(run int64, path, msg string) error {
	_, err := d.db.Exec(`INSERT INTO run_errors(run_id, at, path, error) VALUES(?,?,?,?)`, run, time.Now().UnixNano(), path, msg)
	return err
}

// RunErrors returns up to limit errors of a run.
func (d *DB) RunErrors(run int64, limit int) ([]RunError, error) {
	rows, err := d.db.Query(`SELECT at, path, error FROM run_errors WHERE run_id=? ORDER BY rowid LIMIT ?`, run, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunError
	for rows.Next() {
		var e RunError
		var at int64
		if err := rows.Scan(&at, &e.Path, &e.Error); err != nil {
			return nil, err
		}
		e.At = time.Unix(0, at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune keeps the newest keep runs and their errors.
func (d *DB) Prune(keep int) error {
	_, err := d.db.Exec(`DELETE FROM runs WHERE id <= (SELECT id FROM runs ORDER BY id DESC LIMIT 1 OFFSET ?)`, keep)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(`DELETE FROM run_errors WHERE run_id NOT IN (SELECT id FROM runs)`)
	return err
}

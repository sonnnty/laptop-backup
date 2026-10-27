package sync

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"laptop-backup/internal/config"
	"laptop-backup/internal/drive/drivetest"
	"laptop-backup/internal/index"
)

const rootID = "ROOT_FOLDER_ID_0001"

type env struct {
	t     *testing.T
	ctx   context.Context
	home  string
	src   string
	drive *drivetest.Fake
	idx   *index.DB
	cfg   config.Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join(home, "Documents", "Personal")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := index.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	cfg := config.Default()
	cfg.Sources = []string{src}
	cfg.DriveFolderID = rootID
	return &env{t: t, ctx: context.Background(), home: home, src: src, drive: drivetest.New(rootID), idx: idx, cfg: cfg}
}

func (e *env) write(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.src, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) run(full bool) Stats {
	e.t.Helper()
	s, err := e.runErr(full, nil)
	if err != nil {
		e.t.Fatalf("run: %v", err)
	}
	return s
}

// runErr runs the engine, letting setup adjust it first.
func (e *env) runErr(full bool, setup func(*Engine)) (Stats, error) {
	e.t.Helper()
	id, err := e.idx.StartRun("test", time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	p := &Progress{}
	eng := &Engine{Drive: e.drive, Index: e.idx, Home: e.home, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		RetryBase: time.Millisecond, OfflineWait: time.Millisecond}
	if setup != nil {
		setup(eng)
	}
	err = eng.Run(e.ctx, e.cfg, id, full, p)
	return p.Snapshot(), err
}

// remote returns the content of a mirrored file ("" if absent).
func (e *env) remote(rel string) (string, bool) {
	_, data, ok := e.drive.Lookup(rootID, "Documents/Personal/"+rel)
	return string(data), ok
}

func TestNewFilesAreUploadedUnderMirroredPath(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "hello")
	e.write("sub/b.txt", "world")
	e.write("node_modules/x.js", "skip me")
	e.write("c.tmp", "skip me too")

	s := e.run(false)
	if s.Uploaded != 2 || s.Updated != 0 || s.Trashed != 0 || s.Errors != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if got, _ := e.remote("a.txt"); got != "hello" {
		t.Errorf("a.txt = %q", got)
	}
	if got, _ := e.remote("sub/b.txt"); got != "world" {
		t.Errorf("sub/b.txt = %q", got)
	}
	if _, ok := e.remote("node_modules"); ok {
		t.Error("excluded folder was uploaded")
	}
	if _, ok := e.remote("c.tmp"); ok {
		t.Error("excluded file was uploaded")
	}

	// Nothing changed: the second run makes no Drive writes.
	creates := e.drive.Creates
	s = e.run(false)
	if s.Uploaded+s.Updated+s.Trashed != 0 || e.drive.Creates != creates || e.drive.Gets != 0 {
		t.Fatalf("idle rerun did work: %+v, gets=%d", s, e.drive.Gets)
	}
}

func TestChangedFileIsUpdatedInPlace(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "v1")
	e.run(false)
	before, _, _ := e.drive.Lookup(rootID, "Documents/Personal/a.txt")

	e.write("a.txt", "version 2")
	s := e.run(false)
	if s.Updated != 1 || s.Uploaded != 0 {
		t.Fatalf("stats = %+v", s)
	}
	after, data, _ := e.drive.Lookup(rootID, "Documents/Personal/a.txt")
	if after.ID != before.ID || string(data) != "version 2" {
		t.Fatalf("expected in-place update of %s, got %s %q", before.ID, after.ID, data)
	}
	if n := e.drive.Count("a.txt"); n != 1 {
		t.Fatalf("%d copies of a.txt on Drive", n)
	}
}

func TestMtimeOnlyChangeIsConfirmedByMD5(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "same")
	e.run(false)

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(e.src, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	s := e.run(false)
	if s.Updated != 0 || e.drive.Updates != 0 || e.drive.Gets != 1 {
		t.Fatalf("touch caused upload: %+v updates=%d gets=%d", s, e.drive.Updates, e.drive.Gets)
	}
	// The index now has the new mtime: no further md5 checks.
	e.run(false)
	if e.drive.Gets != 1 {
		t.Fatalf("gets = %d", e.drive.Gets)
	}

	// Same size, different content, new mtime: must be uploaded.
	e.write("a.txt", "SAME")
	even := time.Now().Add(2 * time.Hour)
	os.Chtimes(filepath.Join(e.src, "a.txt"), even, even)
	if s := e.run(false); s.Updated != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if got, _ := e.remote("a.txt"); got != "SAME" {
		t.Fatalf("remote = %q", got)
	}
}

func TestDeletedFilesAndFoldersAreTrashed(t *testing.T) {
	e := newEnv(t)
	e.write("keep.txt", "k")
	e.write("gone.txt", "g")
	e.write("dir/x.txt", "x")
	e.write("dir/deep/y.txt", "y")
	e.run(false)
	gone, _, _ := e.drive.Lookup(rootID, "Documents/Personal/gone.txt")
	dir, _, _ := e.drive.Lookup(rootID, "Documents/Personal/dir")

	os.Remove(filepath.Join(e.src, "gone.txt"))
	os.RemoveAll(filepath.Join(e.src, "dir"))
	s := e.run(false)
	// The folder is trashed as a whole: its children are not trashed one by one.
	if s.Trashed != 2 || e.drive.Trashes != 2 {
		t.Fatalf("stats = %+v, trash calls = %d", s, e.drive.Trashes)
	}
	if !e.drive.IsTrashed(gone.ID) || !e.drive.IsTrashed(dir.ID) {
		t.Fatal("expected items in Drive trash")
	}
	if _, ok := e.remote("keep.txt"); !ok {
		t.Fatal("keep.txt disappeared")
	}
	if n, _ := e.idx.Count(); n != 2 { // source folder + keep.txt
		t.Fatalf("index has %d entries", n)
	}
}

func TestRemovedSourceIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.run(false)
	e.cfg.Sources = nil
	if s := e.run(false); s.Trashed != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if _, ok := e.remote("a.txt"); !ok {
		t.Fatal("Drive copy of removed source was touched")
	}
}

func TestMissingSourceIsNotSwept(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.run(false)
	os.RemoveAll(e.src) // e.g. an unmounted disk
	s := e.run(false)
	if s.Trashed != 0 || s.Errors != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if _, ok := e.remote("a.txt"); !ok {
		t.Fatal("backup was trashed for a missing source")
	}
}

func TestUnreadableFileIsSkippedAndKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}
	e := newEnv(t)
	e.write("a.txt", "a")
	e.write("b.txt", "b")
	e.run(false)
	e.write("a.txt", "a2")
	p := filepath.Join(e.src, "a.txt")
	os.Chmod(p, 0)
	defer os.Chmod(p, 0o644)
	e.write("c.txt", "c")
	s := e.run(false)
	if s.Errors != 1 || s.Uploaded != 1 || s.Trashed != 0 {
		t.Fatalf("stats = %+v", s)
	}
	if got, _ := e.remote("a.txt"); got != "a" {
		t.Fatalf("a.txt = %q", got)
	}
}

func TestSymlinksAreSkipped(t *testing.T) {
	e := newEnv(t)
	e.write("real.txt", "r")
	os.Symlink(filepath.Join(e.src, "real.txt"), filepath.Join(e.src, "link.txt"))
	if s := e.run(false); s.Uploaded != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if _, ok := e.remote("link.txt"); ok {
		t.Fatal("symlink uploaded")
	}
}

func TestFailingFileDoesNotAbortRun(t *testing.T) {
	e := newEnv(t)
	e.write("bad.txt", "b")
	e.write("good.txt", "g")
	e.drive.FailUploads["bad.txt"] = 100
	s := e.run(false)
	if s.Errors != 1 || s.Uploaded != 1 {
		t.Fatalf("stats = %+v", s)
	}
	errs, _ := e.idx.RunErrors(1, 10)
	if len(errs) != 1 || filepath.Base(errs[0].Path) != "bad.txt" {
		t.Fatalf("run errors = %+v", errs)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	e := newEnv(t)
	e.write("flaky.txt", "f")
	e.drive.FailUploads["flaky.txt"] = 2
	e.drive.Transient = true
	if s := e.run(false); s.Errors != 0 || s.Uploaded != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestLostNetworkPausesInsteadOfSkipping(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.drive.FailUploads["a.txt"] = 12 // well past the 5 normal tries
	e.drive.Offline = true
	if s := e.run(false); s.Errors != 0 || s.Uploaded != 1 || s.Paused {
		t.Fatalf("stats = %+v", s)
	}
}

func TestFullRescanRebuildsIndexWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.write("sub/b.txt", "b")
	e.run(false)
	creates := e.drive.Creates

	// Lose the index (e.g. state.db deleted), then rescan.
	e.idx.Clear()
	s := e.run(true)
	if s.Uploaded != 0 || s.Updated != 0 || e.drive.Creates != creates {
		t.Fatalf("rescan re-uploaded: %+v", s)
	}
	if e.drive.Count("a.txt") != 1 || e.drive.Count("b.txt") != 1 {
		t.Fatal("duplicates on Drive")
	}

	// A file that exists only on Drive is trashed by the mirror.
	e.drive.Create(context.Background(), mustFolder(t, e, "Documents/Personal"), "stray.txt", stringsReader("s"), time.Now())
	if s := e.run(true); s.Trashed != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestNewSourceWithExistingDriveFolderIsAdopted(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.run(false)

	// Same Drive, fresh index: the existing remote folder is indexed first.
	idx2, _ := index.Open(filepath.Join(t.TempDir(), "state.db"))
	defer idx2.Close()
	e.idx = idx2
	if s := e.run(false); s.Uploaded != 0 || e.drive.Count("a.txt") != 1 {
		t.Fatalf("stats = %+v, copies = %d", s, e.drive.Count("a.txt"))
	}
}

func TestFileTrashedOnDriveIsReuploaded(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "a")
	e.run(false)
	f, _, _ := e.drive.Lookup(rootID, "Documents/Personal/a.txt")
	e.drive.Trash(context.Background(), f.ID)

	e.write("a.txt", "a changed")
	if s := e.run(false); s.Uploaded != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if got, _ := e.remote("a.txt"); got != "a changed" {
		t.Fatalf("remote = %q", got)
	}
}

func TestFileReplacedByFolder(t *testing.T) {
	e := newEnv(t)
	e.write("x", "file")
	e.run(false)
	os.Remove(filepath.Join(e.src, "x"))
	e.write("x/inner.txt", "i")
	s := e.run(false)
	if s.Trashed != 1 || s.Uploaded != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if got, _ := e.remote("x/inner.txt"); got != "i" {
		t.Fatalf("remote = %q", got)
	}
}

func TestOutsideHomeMapsUnderRoot(t *testing.T) {
	e := newEnv(t)
	other := t.TempDir() // not under e.home
	os.WriteFile(filepath.Join(other, "o.txt"), []byte("o"), 0o644)
	e.cfg.Sources = []string{other}
	e.run(false)
	rel := filepath.Join(RemotePath(e.home, other)...)
	if _, data, ok := e.drive.Lookup(rootID, rel+"/o.txt"); !ok || string(data) != "o" {
		t.Fatalf("missing %s/o.txt", rel)
	}
}

func mustFolder(t *testing.T, e *env, path string) string {
	f, _, ok := e.drive.Lookup(rootID, path)
	if !ok {
		t.Fatalf("no folder %s", path)
	}
	return f.ID
}

type sr struct{ s string }

func (r *sr) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

func stringsReader(s string) io.Reader { return &sr{s} }

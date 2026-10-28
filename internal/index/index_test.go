package index

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSubtreeQueries(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Siblings that sort between "/a/b" and its children must not leak in.
	for _, p := range []string{"/a/b", "/a/b/c", "/a/b/c/d", "/a/b.txt", "/a/b-x", "/a/b0", "/a/bc"} {
		if err := db.Put(Entry{Path: p, DriveID: "id" + p}, 1); err != nil {
			t.Fatal(err)
		}
	}
	db.MarkSeen("/a/b/c", 2)
	got, err := db.Unseen("/a/b", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/a/b/c/d" {
		t.Fatalf("Unseen = %+v", got)
	}
	db.DeleteSubtree("/a/b")
	if n, _ := db.Count(); n != 5 {
		t.Fatalf("count after DeleteSubtree = %d", n)
	}
}

func TestRunsAndLastSuccess(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	id1, _ := db.StartRun("schedule", t0)
	db.FinishRun(Run{ID: id1, Status: StatusSuccess, Finished: t0.Add(time.Minute)})
	id2, _ := db.StartRun("manual", t0.Add(time.Hour))
	db.FinishRun(Run{ID: id2, Status: StatusFailed, Finished: t0.Add(time.Hour)})
	db.AddRunError(id2, "/x", "boom")

	start, success, err := db.LastRuns()
	if err != nil || !start.Equal(t0.Add(time.Hour)) || !success.Equal(t0) {
		t.Fatalf("LastRuns = %v %v %v", start, success, err)
	}
	runs, _ := db.RecentRuns(10)
	if len(runs) != 2 || runs[0].ID != id2 || runs[1].Duration() != time.Minute {
		t.Fatalf("RecentRuns = %+v", runs)
	}
	if errs, _ := db.RunErrors(id2, 10); len(errs) != 1 || errs[0].Error != "boom" {
		t.Fatalf("RunErrors = %+v", errs)
	}
	db.Prune(1)
	if runs, _ := db.RecentRuns(10); len(runs) != 1 {
		t.Fatalf("after prune: %d runs", len(runs))
	}
}

func TestRunsBefore(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t0 := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 7 {
		id, err := db.StartRun("schedule", t0.Add(time.Duration(i)*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	db.Prune(6) // drops ids[0]: cursors must not assume contiguous IDs

	var got []int64
	before := int64(0)
	for pages := 0; ; pages++ {
		runs, more, err := db.RunsBefore(before, 4)
		if err != nil || pages > 2 {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, r := range runs {
			got = append(got, r.ID)
		}
		if !more {
			if len(runs) != 2 {
				t.Fatalf("last page has %d runs", len(runs))
			}
			break
		}
		if len(runs) != 4 {
			t.Fatalf("full page has %d runs", len(runs))
		}
		before = runs[len(runs)-1].ID
	}
	want := []int64{ids[6], ids[5], ids[4], ids[3], ids[2], ids[1]}
	if !slices.Equal(got, want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}

	// An exact multiple of the page size: the second page is empty, not "more".
	if runs, more, _ := db.RunsBefore(0, 6); len(runs) != 6 || more {
		t.Fatalf("RunsBefore(0, 6) = %d runs, more=%v", len(runs), more)
	}
	if runs, more, _ := db.RunsBefore(ids[1], 6); len(runs) != 0 || more {
		t.Fatalf("past the end = %d runs, more=%v", len(runs), more)
	}
	// A run started meanwhile does not shift an older page.
	db.StartRun("manual", t0.Add(24*time.Hour))
	if runs, _, _ := db.RunsBefore(ids[3], 2); len(runs) != 2 || runs[0].ID != ids[2] {
		t.Fatalf("RunsBefore(ids[3]) = %+v", runs)
	}
}

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSourceUsage(t *testing.T) {
	db := openTest(t)
	for _, e := range []Entry{
		{Path: "/a/b", IsDir: true, Size: 4096}, // the source folder itself: not a file
		{Path: "/a/b/x", Size: 10},
		{Path: "/a/b/sub", IsDir: true, Size: 4096},
		{Path: "/a/b/sub/y", Size: 200},
		// Siblings whose names sort right after "/a/b" must not be counted.
		{Path: "/a/b.txt", Size: 1000}, {Path: "/a/b-x", Size: 1000}, {Path: "/a/b0", Size: 1000},
		{Path: "/a/bc", IsDir: true}, {Path: "/a/bc/z", Size: 5000},
		{Path: "/a/b/empty"},
	} {
		e.DriveID = "id" + e.Path
		if err := db.Put(e, 1); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := db.SourceUsage("/a/b"); err != nil || u != (Usage{Files: 3, Bytes: 210}) {
		t.Fatalf("/a/b = %+v, %v", u, err)
	}
	if u, _ := db.SourceUsage("/a/bc"); u != (Usage{Files: 1, Bytes: 5000}) {
		t.Fatalf("/a/bc = %+v", u)
	}
	if u, _ := db.SourceUsage("/nothing"); u != (Usage{}) {
		t.Fatalf("empty source = %+v", u)
	}
	if u, _ := db.SourceUsage("/"); u.Files != 7 {
		t.Fatalf("/ = %+v", u)
	}
	if u, err := db.TotalUsage(); err != nil || u != (Usage{Files: 7, Bytes: 8210}) {
		t.Fatalf("total = %+v, %v", u, err)
	}
}

func TestBytesPerDay(t *testing.T) {
	db := openTest(t)
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	add := func(started time.Time, status string, bytes int64) {
		id, err := db.StartRun("schedule", started)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishRun(Run{ID: id, Status: status, Finished: started.Add(time.Minute), Bytes: bytes}); err != nil {
			t.Fatal(err)
		}
	}
	add(now, StatusSuccess, 100)                                               // today
	add(now.Add(-3*time.Hour), StatusPartial, 50)                              // today, earlier
	add(time.Date(2026, 10, 27, 0, 0, 0, 0, time.Local), StatusSuccess, 7)     // yesterday, first instant
	add(time.Date(2026, 10, 26, 23, 59, 59, 0, time.Local), StatusFailed, 3)   // 2 days ago, last instant
	add(time.Date(2026, 10, 24, 9, 0, 0, 0, time.Local), StatusSuccess, 1000)  // 4 days ago
	add(time.Date(2026, 9, 28, 23, 59, 0, 0, time.Local), StatusSuccess, 9999) // before the 30-day window
	add(time.Date(2026, 9, 29, 0, 0, 1, 0, time.Local), StatusSuccess, 5)      // first day of the window

	got, err := db.BytesPerDay(now, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 30 {
		t.Fatalf("%d days", len(got))
	}
	if first := got[0].Day; first.Format("2006-01-02") != "2026-09-29" || got[29].Day.Format("2006-01-02") != "2026-10-28" {
		t.Fatalf("range %v .. %v", first, got[29].Day)
	}
	want := map[string]int64{"2026-09-29": 5, "2026-10-24": 1000, "2026-10-26": 3, "2026-10-27": 7, "2026-10-28": 150}
	var total int64
	for _, d := range got {
		key := d.Day.Format("2006-01-02")
		if d.Bytes != want[key] {
			t.Errorf("%s = %d, want %d", key, d.Bytes, want[key])
		}
		total += d.Bytes
	}
	if total != 1165 {
		t.Errorf("total %d (a run outside the window was counted?)", total)
	}
	if empty, _ := openTest(t).BytesPerDay(now, 3); len(empty) != 3 || empty[0].Bytes != 0 {
		t.Errorf("empty db: %+v", empty)
	}
}

func TestFinishedRunsAndCounts(t *testing.T) {
	db := openTest(t)
	base := time.Now().Add(-48 * time.Hour)
	for i, st := range []string{StatusSuccess, StatusFailed, StatusPartial, StatusSuccess} {
		id, _ := db.StartRun("manual", base.Add(time.Duration(i)*time.Hour))
		db.FinishRun(Run{ID: id, Status: st, Finished: base.Add(time.Duration(i)*time.Hour + time.Minute)})
	}
	db.StartRun("manual", time.Now()) // still running: never charted
	runs, err := db.FinishedRuns(3)
	if err != nil || len(runs) != 3 || runs[0].ID != 2 || runs[2].ID != 4 {
		t.Fatalf("FinishedRuns = %+v, %v", runs, err)
	}
	counts, err := db.RunCounts(base.Add(90 * time.Minute)) // excludes runs 1 and 2
	if err != nil || len(counts) != 2 || counts[StatusPartial] != 1 || counts[StatusSuccess] != 1 {
		t.Fatalf("RunCounts = %v, %v", counts, err)
	}
}

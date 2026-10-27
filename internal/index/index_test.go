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

package sync

import (
	"slices"
	"testing"

	"laptop-backup/internal/config"
)

func TestRemotePath(t *testing.T) {
	home := "/home/alice"
	tests := []struct {
		dir  string
		want []string
	}{
		{"/home/alice/Documents/Personal", []string{"Documents", "Personal"}},
		{"/home/alice/Documents/", []string{"Documents"}},
		{"/home/alice", nil},
		{"/home/alicex/data", []string{"_root", "home", "alicex", "data"}},
		{"/mnt/data/photos", []string{"_root", "mnt", "data", "photos"}},
		{"/", []string{"_root"}},
	}
	for _, tt := range tests {
		if got := RemotePath(home, tt.dir); !slices.Equal(got, tt.want) {
			t.Errorf("RemotePath(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestNormalizeSources(t *testing.T) {
	got := NormalizeSources([]string{"/home/a/x/", "/home/a/x/y", "/home/a/x y", "relative", "/home/a/x", "/srv"})
	want := []string{"/home/a/x", "/home/a/x y", "/srv"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExcluded(t *testing.T) {
	p := slices.Concat(config.DefaultExcludes, []string{"build/*.o", "/secret.txt", "# comment", ""})
	tests := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"app/node_modules", true, true},
		{"app/node_modules", false, false}, // "node_modules/" is directory-only
		{"proj/.venv", true, true},
		{"proj/venv", true, true},
		{"pkg/__pycache__", true, true},
		{".cache", true, true},
		{"a/b/file.tmp", false, true},
		{"a/.notes.txt.swp", false, true},
		{".Trash-1000", true, true},
		{"a/file.txt", false, false},
		{"src/main.go", false, false},
		{"build/x.o", false, true},
		{"sub/build/x.o", false, false},
		{"secret.txt", false, true},
		{"sub/secret.txt", false, false},
		{"tmp", true, false},
		{"proj/.git", true, true},
		{"go/vendor", true, true},
		{"web/.next", true, true},
		{"web/dist", true, true},
		{"app/build", true, true},
		{"infra/.terraform", true, true},
		{"notes/dist", false, false}, // directory-only
	}
	for _, tt := range tests {
		if got := Excluded(p, tt.rel, tt.isDir); got != tt.want {
			t.Errorf("Excluded(%q, dir=%v) = %v, want %v", tt.rel, tt.isDir, got, tt.want)
		}
	}
}

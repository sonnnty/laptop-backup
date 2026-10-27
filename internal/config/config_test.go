package config

import (
	"os"
	"path/filepath"
	"testing"
)

const testFolderID = "testDriveFolder_0123456789"

func TestOldConfigGetsUploadDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	old := `{"sources":[],"excludes":[],"schedule":{"mode":"paused","every_hours":6,"daily_at":"02:00"},"drive_folder_id":"` + testFolderID + `"}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Get(); c.UploadWorkers != DefaultUploadWorkers || c.UploadLimitKBps != 0 {
		t.Fatalf("workers=%d limit=%d", c.UploadWorkers, c.UploadLimitKBps)
	}
	if got := s.Get().DriveFolderID; got != testFolderID {
		t.Fatalf("existing folder ID changed to %q", got)
	}
}

func TestEmptyDriveFolderID(t *testing.T) {
	if id := Default().DriveFolderID; id != "" {
		t.Fatalf("default folder ID = %q, want empty", id)
	}
	// A fresh install (no file) and a saved config without an ID both load.
	dir := t.TempDir()
	s, err := Load(filepath.Join(dir, "missing.json"))
	if err != nil || s.Get().DriveFolderID != "" {
		t.Fatalf("Load(missing) = %v, %v", s, err)
	}
	p := filepath.Join(dir, "config.json")
	if err := s.Update(func(c *Config) {}); err != nil {
		t.Fatal(err)
	}
	cfg := `{"sources":[],"excludes":[],"schedule":{"mode":"interval","every_hours":6,"daily_at":"02:00"},"drive_folder_id":""}`
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("empty folder ID rejected on load: %v", err)
	}
	// A non-empty ID must still look like a Drive ID.
	c := Default()
	c.DriveFolderID = "bad id!"
	if c.Validate() == nil {
		t.Fatal("invalid folder ID accepted")
	}
	c.DriveFolderID = testFolderID
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateUploadSettings(t *testing.T) {
	for _, tt := range []struct {
		workers, limit int
		ok             bool
	}{
		{1, 0, true}, {8, 0, true}, {4, 512, true}, {4, MaxUploadLimitKBps, true},
		{0, 0, false}, {9, 0, false}, {4, -1, false}, {4, MaxUploadLimitKBps + 1, false},
	} {
		c := Default()
		c.UploadWorkers, c.UploadLimitKBps = tt.workers, tt.limit
		if err := c.Validate(); (err == nil) != tt.ok {
			t.Errorf("workers=%d limit=%d: err=%v", tt.workers, tt.limit, err)
		}
	}
}

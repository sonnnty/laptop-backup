// Package config loads and saves ~/.config/laptop-backup/config.json and
// resolves the service's file locations.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"
)

// DefaultExcludes are applied to every source folder.
var DefaultExcludes = []string{
	"node_modules/", ".venv/", "venv/", "__pycache__/", ".cache/",
	"*.tmp", "*.swp", ".Trash-*/",
	"vendor/", ".git/", ".next/", "dist/", "build/", ".terraform/",
}

// Upload tuning limits.
const (
	DefaultUploadWorkers = 4
	MaxUploadWorkers     = 8
	// MaxUploadLimitKBps keeps KiB/s → bytes/s far from overflowing.
	MaxUploadLimitKBps = 1 << 20
)

// Schedule modes.
const (
	ModeInterval = "interval" // every EveryHours hours
	ModeDaily    = "daily"    // every day at DailyAt (local time)
	ModePaused   = "paused"
)

type Schedule struct {
	Mode       string `json:"mode"`
	EveryHours int    `json:"every_hours"`
	DailyAt    string `json:"daily_at"` // "HH:MM"
}

type Config struct {
	Sources  []string `json:"sources"`
	Excludes []string `json:"excludes"`
	Schedule Schedule `json:"schedule"`
	// DriveFolderID is the Drive folder that receives the mirror. It is
	// empty until the user sets it in the UI; runs refuse to start then.
	DriveFolderID string `json:"drive_folder_id"`
	// UploadWorkers is how many files are uploaded at the same time.
	UploadWorkers int `json:"upload_workers"`
	// UploadLimitKBps caps the total upload speed in KiB/s (0 = unlimited).
	UploadLimitKBps int `json:"upload_limit_kbps"`
}

func Default() Config {
	return Config{
		Sources:       []string{},
		Excludes:      slices.Clone(DefaultExcludes),
		Schedule:      Schedule{Mode: ModeInterval, EveryHours: 6, DailyAt: "02:00"},
		UploadWorkers: DefaultUploadWorkers,
	}
}

var driveIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,}$`)

// ValidDriveID reports whether id looks like a Drive file/folder ID.
func ValidDriveID(id string) bool { return driveIDRe.MatchString(id) }

func (s Schedule) Validate() error {
	switch s.Mode {
	case ModeInterval:
		if s.EveryHours < 1 || s.EveryHours > 24 {
			return errors.New("ช่วงเวลาต้องอยู่ระหว่าง 1–24 ชั่วโมง")
		}
	case ModeDaily:
		if _, err := time.Parse("15:04", s.DailyAt); err != nil {
			return errors.New("เวลาต้องอยู่ในรูปแบบ HH:MM")
		}
	case ModePaused:
	default:
		return fmt.Errorf("unknown schedule mode %q", s.Mode)
	}
	return nil
}

func (c Config) Validate() error {
	for _, s := range c.Sources {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s {
			return fmt.Errorf("source must be a clean absolute path: %q", s)
		}
	}
	// Empty means "not set yet" (fresh install); a run checks for it.
	if c.DriveFolderID != "" && !ValidDriveID(c.DriveFolderID) {
		return errors.New("Drive folder ID ไม่ถูกต้อง")
	}
	if c.UploadWorkers < 1 || c.UploadWorkers > MaxUploadWorkers {
		return fmt.Errorf("จำนวนไฟล์ที่อัปโหลดพร้อมกันต้องอยู่ระหว่าง 1–%d", MaxUploadWorkers)
	}
	if c.UploadLimitKBps < 0 || c.UploadLimitKBps > MaxUploadLimitKBps {
		return fmt.Errorf("ความเร็วอัปโหลดต้องอยู่ระหว่าง 0–%d KB/s (0 = ไม่จำกัด)", MaxUploadLimitKBps)
	}
	return c.Schedule.Validate()
}

func (c Config) clone() Config {
	c.Sources = slices.Clone(c.Sources)
	c.Excludes = slices.Clone(c.Excludes)
	return c
}

// Store guards the config in memory and persists every change.
type Store struct {
	path string
	mu   sync.Mutex
	cfg  Config
}

// Load reads path, or returns defaults (without writing) when it is missing.
func Load(path string) (*Store, error) {
	s := &Store{path: path, cfg: Default()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.cfg.Sources == nil {
		s.cfg.Sources = []string{}
	}
	if err := s.cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.clone()
}

// Update applies fn to a copy of the config, validates and saves it.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg.clone()
	fn(&c)
	if err := c.Validate(); err != nil {
		return err
	}
	if err := WriteJSON(s.path, c, 0o644); err != nil {
		return err
	}
	s.cfg = c
	return nil
}

// Save writes the current config to disk.
func (s *Store) Save() error { return s.Update(func(*Config) {}) }

// WriteJSON atomically writes v as indented JSON with the given mode.
func WriteJSON(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Paths holds the service's directories.
type Paths struct {
	Home      string
	ConfigDir string // ~/.config/laptop-backup
	DataDir   string // ~/.local/share/laptop-backup
}

// DefaultPaths honours $XDG_CONFIG_HOME and $XDG_DATA_HOME.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(cfg) {
		cfg = filepath.Join(home, ".config")
	}
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		data = filepath.Join(home, ".local", "share")
	}
	return Paths{
		Home:      filepath.Clean(home),
		ConfigDir: filepath.Join(cfg, "laptop-backup"),
		DataDir:   filepath.Join(data, "laptop-backup"),
	}, nil
}

func (p Paths) ConfigFile() string      { return filepath.Join(p.ConfigDir, "config.json") }
func (p Paths) CredentialsFile() string { return filepath.Join(p.ConfigDir, "credentials.json") }
func (p Paths) TokenFile() string       { return filepath.Join(p.ConfigDir, "token.json") }
func (p Paths) StateDB() string         { return filepath.Join(p.DataDir, "state.db") }
func (p Paths) LogFile() string         { return filepath.Join(p.DataDir, "laptop-backup.log") }

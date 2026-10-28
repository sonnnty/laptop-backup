// Package drivetest provides an in-memory drive.Client for tests.
package drivetest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"laptop-backup/internal/drive"
)

type node struct {
	drive.File
	Parent string
	Data   []byte
}

// Fake is a goroutine-safe in-memory Drive.
type Fake struct {
	mu     sync.Mutex
	nodes  map[string]*node
	nextID int

	// FailUploads makes the next N Create/Update calls for a file name
	// fail; Transient selects a retryable error.
	FailUploads map[string]int
	Transient   bool
	Offline     bool // failed uploads return drive.ErrOffline
	// FailWith makes every Create, Update or Trash of an item with that
	// name return the error.
	FailWith map[string]error
	// Latency is added to every call (outside the lock, so concurrent
	// calls overlap like real network round trips).
	Latency time.Duration

	// QuotaValue and QuotaErr are returned by Quota.
	QuotaValue drive.Quota
	QuotaErr   error
	Quotas     int // calls to Quota

	Creates, Updates, Trashes, Gets int
	// MaxInFlight is the highest number of calls seen at the same time.
	MaxInFlight int
	inFlight    int
}

// New returns a fake with one root folder whose ID is rootID.
func New(rootID string) *Fake {
	f := &Fake{nodes: map[string]*node{}, FailUploads: map[string]int{}, FailWith: map[string]error{}}
	f.nodes[rootID] = &node{File: drive.File{ID: rootID, Name: "root", Folder: true}}
	return f
}

func (f *Fake) newID() string {
	f.nextID++
	return fmt.Sprintf("id%04d", f.nextID)
}

// call simulates the round trip of one API call; it returns with f.mu held
// unless the context ends first.
func (f *Fake) call(ctx context.Context) error {
	f.mu.Lock()
	f.inFlight++
	f.MaxInFlight = max(f.MaxInFlight, f.inFlight)
	lat := f.Latency
	f.mu.Unlock()
	var err error
	if lat > 0 {
		t := time.NewTimer(lat)
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-t.C:
		}
		t.Stop()
	}
	f.mu.Lock()
	f.inFlight--
	if err != nil {
		f.mu.Unlock()
	}
	return err
}

func (f *Fake) FindFolder(ctx context.Context, parentID, name string) (string, bool, error) {
	if err := f.call(ctx); err != nil {
		return "", false, err
	}
	defer f.mu.Unlock()
	for _, n := range f.sortedChildren(parentID) {
		if n.Folder && n.Name == name && !n.Trashed {
			return n.ID, true, nil
		}
	}
	return "", false, nil
}

func (f *Fake) CreateFolder(ctx context.Context, parentID, name string) (string, error) {
	if err := f.call(ctx); err != nil {
		return "", err
	}
	defer f.mu.Unlock()
	if _, ok := f.nodes[parentID]; !ok {
		return "", drive.ErrNotFound
	}
	id := f.newID()
	f.nodes[id] = &node{File: drive.File{ID: id, Name: name, Folder: true}, Parent: parentID}
	return id, nil
}

func (f *Fake) failUpload(name string) error {
	if err := f.FailWith[name]; err != nil {
		return err
	}
	if f.FailUploads[name] > 0 {
		f.FailUploads[name]--
		if f.Offline {
			return drive.ErrOffline
		}
		if f.Transient {
			return drive.ErrTransient
		}
		return fmt.Errorf("permanent failure for %s", name)
	}
	return nil
}

func (f *Fake) setData(n *node, media io.Reader, modTime time.Time) error {
	data, err := io.ReadAll(media)
	if err != nil {
		return err
	}
	sum := md5.Sum(data)
	n.Data, n.Size, n.MD5, n.ModTime = data, int64(len(data)), hex.EncodeToString(sum[:]), modTime
	return nil
}

func (f *Fake) Create(ctx context.Context, parentID, name string, media io.Reader, modTime time.Time) (drive.File, error) {
	if err := f.call(ctx); err != nil {
		return drive.File{}, err
	}
	defer f.mu.Unlock()
	if err := f.failUpload(name); err != nil {
		return drive.File{}, err
	}
	if _, ok := f.nodes[parentID]; !ok {
		return drive.File{}, drive.ErrNotFound
	}
	f.Creates++
	n := &node{File: drive.File{ID: f.newID(), Name: name}, Parent: parentID}
	if err := f.setData(n, media, modTime); err != nil {
		return drive.File{}, err
	}
	f.nodes[n.ID] = n
	return n.File, nil
}

func (f *Fake) Update(ctx context.Context, id string, media io.Reader, modTime time.Time) (drive.File, error) {
	if err := f.call(ctx); err != nil {
		return drive.File{}, err
	}
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok {
		return drive.File{}, drive.ErrNotFound
	}
	if err := f.failUpload(n.Name); err != nil {
		return drive.File{}, err
	}
	f.Updates++
	if err := f.setData(n, media, modTime); err != nil {
		return drive.File{}, err
	}
	return n.File, nil
}

func (f *Fake) Get(ctx context.Context, id string) (drive.File, error) {
	if err := f.call(ctx); err != nil {
		return drive.File{}, err
	}
	defer f.mu.Unlock()
	f.Gets++
	n, ok := f.nodes[id]
	if !ok {
		return drive.File{}, drive.ErrNotFound
	}
	file := n.File
	file.Trashed = f.trashed(n)
	file.CanAddChildren = n.Folder
	return file, nil
}

func (f *Fake) Trash(ctx context.Context, id string) error {
	if err := f.call(ctx); err != nil {
		return err
	}
	defer f.mu.Unlock()
	n, ok := f.nodes[id]
	if !ok {
		return drive.ErrNotFound
	}
	if err := f.FailWith[n.Name]; err != nil {
		return err
	}
	f.Trashes++
	n.Trashed = true
	return nil
}

func (f *Fake) List(ctx context.Context, folderID string, fn func(drive.File) error) error {
	if err := f.call(ctx); err != nil {
		return err
	}
	var out []drive.File
	for _, n := range f.sortedChildren(folderID) {
		if !n.Trashed {
			out = append(out, n.File)
		}
	}
	f.mu.Unlock()
	for _, file := range out {
		if err := fn(file); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) Quota(ctx context.Context) (drive.Quota, error) {
	if err := f.call(ctx); err != nil {
		return drive.Quota{}, err
	}
	defer f.mu.Unlock()
	f.Quotas++
	return f.QuotaValue, f.QuotaErr
}

func (f *Fake) sortedChildren(parent string) []*node {
	var out []*node
	for _, n := range f.nodes {
		if n.Parent == parent {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// trashed reports whether n or one of its ancestors is trashed.
func (f *Fake) trashed(n *node) bool {
	for ; n != nil; n = f.nodes[n.Parent] {
		if n.Trashed {
			return true
		}
	}
	return false
}

// Lookup resolves a slash-separated path below rootID, ignoring trashed
// items, and returns the file and its content.
func (f *Fake) Lookup(rootID, path string) (drive.File, []byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.nodes[rootID]
	for _, part := range strings.Split(path, "/") {
		var next *node
		for _, n := range f.sortedChildren(cur.ID) {
			if n.Name == part && !n.Trashed {
				next = n
				break
			}
		}
		if next == nil {
			return drive.File{}, nil, false
		}
		cur = next
	}
	return cur.File, cur.Data, true
}

// IsTrashed reports whether the item (or an ancestor) is in the trash.
func (f *Fake) IsTrashed(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trashed(f.nodes[id])
}

// Count returns the number of non-trashed items with the given name.
func (f *Fake) Count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := 0
	for _, n := range f.nodes {
		if n.Name == name && !f.trashed(n) {
			c++
		}
	}
	return c
}

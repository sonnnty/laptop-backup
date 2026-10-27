// Package drive is the small slice of the Google Drive API the mirror needs,
// behind an interface so the sync logic can be tested with a fake.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const FolderMime = "application/vnd.google-apps.folder"

// File is the metadata the mirror cares about.
type File struct {
	ID      string
	Name    string
	MD5     string
	Size    int64
	Folder  bool
	Trashed bool
	ModTime time.Time
	// CanAddChildren is only filled by Get (used by "test access").
	CanAddChildren bool
}

// Client is implemented by *GDrive and by drivetest.Fake.
type Client interface {
	// FindFolder returns the ID of the non-trashed folder name in parent.
	FindFolder(ctx context.Context, parentID, name string) (id string, found bool, err error)
	CreateFolder(ctx context.Context, parentID, name string) (string, error)
	// Create uploads a new file; Update replaces the content of an existing
	// file in place (same ID). Both set Drive's modifiedTime to modTime.
	Create(ctx context.Context, parentID, name string, media io.Reader, modTime time.Time) (File, error)
	Update(ctx context.Context, id string, media io.Reader, modTime time.Time) (File, error)
	Get(ctx context.Context, id string) (File, error)
	// Trash moves a file or folder to the Drive trash (never deletes).
	Trash(ctx context.Context, id string) error
	// List calls fn for every non-trashed child of folderID.
	List(ctx context.Context, folderID string, fn func(File) error) error
}

// ErrNotFound is returned when an ID no longer exists.
var ErrNotFound = errors.New("drive: file not found")

// ErrTransient marks an error worth retrying (used by the fake client).
var ErrTransient = errors.New("drive: transient error")

// ErrOffline marks a lost network connection (used by the fake client).
var ErrOffline = errors.New("drive: network unreachable")

// Retryable reports whether err is a rate-limit, server or network error
// that may succeed on retry.
func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrTransient) || IsNetwork(err) {
		return true
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		if ge.Code == http.StatusTooManyRequests || ge.Code >= 500 {
			return true
		}
		if ge.Code == http.StatusForbidden {
			for _, e := range ge.Errors {
				if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
					return true
				}
			}
		}
		return false
	}
	return false
}

// IsNetwork reports whether err is a transport failure (no route, reset,
// timeout) rather than an answer from Google. A refused token
// (invalid_grant) is an answer, so it is not a network error.
func IsNetwork(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrOffline) {
		return true
	}
	var ge *googleapi.Error
	var re *oauth2.RetrieveError
	if errors.As(err, &ge) || errors.As(err, &re) {
		return false
	}
	var ue *url.Error
	return errors.As(err, &ue) || errors.Is(err, io.ErrUnexpectedEOF)
}

// IsStorageFull reports whether the Drive owner is out of storage quota;
// every further upload would fail the same way.
func IsStorageFull(err error) bool {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		for _, e := range ge.Errors {
			if e.Reason == "storageQuotaExceeded" {
				return true
			}
		}
	}
	return false
}

// uploadChunk is the resumable-upload threshold and chunk size: files larger
// than this use a resumable session, and at most one chunk is buffered.
const uploadChunk = 5 << 20

const fileFields = "id,name,md5Checksum,size,mimeType,trashed,modifiedTime"

// GDrive talks to the real Drive API.
type GDrive struct{ svc *drive.Service }

// New wraps an OAuth-authorised HTTP client.
func New(ctx context.Context, hc *http.Client) (*GDrive, error) {
	svc, err := drive.NewService(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return nil, err
	}
	return &GDrive{svc: svc}, nil
}

func quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func toFile(f *drive.File) File {
	out := File{ID: f.Id, Name: f.Name, MD5: f.Md5Checksum, Size: f.Size, Folder: f.MimeType == FolderMime, Trashed: f.Trashed}
	out.ModTime, _ = time.Parse(time.RFC3339Nano, f.ModifiedTime)
	if f.Capabilities != nil {
		out.CanAddChildren = f.Capabilities.CanAddChildren
	}
	return out
}

func wrap(err error) error {
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == http.StatusNotFound {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

func (g *GDrive) FindFolder(ctx context.Context, parentID, name string) (string, bool, error) {
	q := fmt.Sprintf("%s in parents and name = %s and mimeType = '%s' and trashed = false", quote(parentID), quote(name), FolderMime)
	r, err := g.svc.Files.List().Q(q).Fields("files(id)").PageSize(10).
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).Context(ctx).Do()
	if err != nil {
		return "", false, wrap(err)
	}
	if len(r.Files) == 0 {
		return "", false, nil
	}
	return r.Files[0].Id, true, nil
}

func (g *GDrive) CreateFolder(ctx context.Context, parentID, name string) (string, error) {
	f, err := g.svc.Files.Create(&drive.File{Name: name, MimeType: FolderMime, Parents: []string{parentID}}).
		Fields("id").SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return "", wrap(err)
	}
	return f.Id, nil
}

func (g *GDrive) Create(ctx context.Context, parentID, name string, media io.Reader, modTime time.Time) (File, error) {
	meta := &drive.File{Name: name, Parents: []string{parentID}, ModifiedTime: modTime.UTC().Format(time.RFC3339Nano)}
	f, err := g.svc.Files.Create(meta).Media(media, googleapi.ChunkSize(uploadChunk)).
		Fields(fileFields).SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return File{}, wrap(err)
	}
	return toFile(f), nil
}

func (g *GDrive) Update(ctx context.Context, id string, media io.Reader, modTime time.Time) (File, error) {
	meta := &drive.File{ModifiedTime: modTime.UTC().Format(time.RFC3339Nano)}
	f, err := g.svc.Files.Update(id, meta).Media(media, googleapi.ChunkSize(uploadChunk)).
		Fields(fileFields).SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return File{}, wrap(err)
	}
	return toFile(f), nil
}

func (g *GDrive) Get(ctx context.Context, id string) (File, error) {
	f, err := g.svc.Files.Get(id).Fields(fileFields + ",capabilities/canAddChildren").
		SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return File{}, wrap(err)
	}
	return toFile(f), nil
}

func (g *GDrive) Trash(ctx context.Context, id string) error {
	_, err := g.svc.Files.Update(id, &drive.File{Trashed: true}).Fields("id").
		SupportsAllDrives(true).Context(ctx).Do()
	return wrap(err)
}

func (g *GDrive) List(ctx context.Context, folderID string, fn func(File) error) error {
	q := fmt.Sprintf("%s in parents and trashed = false", quote(folderID))
	return wrap(g.svc.Files.List().Q(q).Fields("nextPageToken,files("+fileFields+")").PageSize(1000).
		SupportsAllDrives(true).IncludeItemsFromAllDrives(true).Context(ctx).
		Pages(ctx, func(r *drive.FileList) error {
			for _, f := range r.Files {
				if err := fn(toFile(f)); err != nil {
					return err
				}
			}
			return nil
		}))
}

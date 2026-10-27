package drive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"laptop-backup/internal/ratelimit"
)

// resumableServer speaks just enough of Drive's resumable upload protocol
// for one file.
type resumableServer struct {
	mu   stdsync.Mutex
	data bytes.Buffer
	puts int // chunk requests
}

func (s *resumableServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Query().Get("uploadType") == "resumable":
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "http://"+r.Host+"/session")
		w.WriteHeader(http.StatusOK)
	case r.URL.Path == "/session": // the Go client sends chunks with POST
		s.puts++
		io.Copy(&s.data, r.Body)
		var lo, hi int64
		var total string
		fmt.Sscanf(strings.NewReplacer("-", " ", "/", " ").Replace(r.Header.Get("Content-Range")), "bytes %d %d %s", &lo, &hi, &total)
		if total == "*" {
			// "308 Resume Incomplete", as sent to clients with X-GUploader-No-308.
			w.Header().Set("X-Http-Status-Code-Override", "308")
			w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", hi))
			return
		}
		sum := md5.Sum(s.data.Bytes())
		fmt.Fprintf(w, `{"id":"f1","name":"big.bin","md5Checksum":%q,"size":"%d"}`, hex.EncodeToString(sum[:]), s.data.Len())
	default:
		http.Error(w, r.Method+" "+r.URL.String(), http.StatusNotFound)
	}
}

// TestThrottledResumableUpload sends a file larger than one chunk through
// a rate-limited reader: the chunked (resumable) upload must still work.
func TestThrottledResumableUpload(t *testing.T) {
	rs := &resumableServer{}
	srv := httptest.NewServer(rs)
	defer srv.Close()
	ctx := context.Background()
	svc, err := drive.NewService(ctx, option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	g := &GDrive{svc: svc}

	content := make([]byte, uploadChunk+uploadChunk/2)
	for i := range content {
		content[i] = byte(rand.N(256))
	}
	var lim ratelimit.Limiter
	lim.SetRate(64 << 20) // fast, but every Read goes through the bucket
	f, err := g.Create(ctx, "parent", "big.bin", lim.Reader(ctx, bytes.NewReader(content)), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(content)
	if f.MD5 != hex.EncodeToString(sum[:]) || f.Size != int64(len(content)) {
		t.Fatalf("file = %+v", f)
	}
	if rs.puts < 2 || !bytes.Equal(rs.data.Bytes(), content) {
		t.Fatalf("puts = %d, got %d bytes", rs.puts, rs.data.Len())
	}
}

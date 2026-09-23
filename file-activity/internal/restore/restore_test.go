package restore

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree/treetest"
)

const sp = "space-1"

// fakeBlobs is the bucket of the platform in a map.
type fakeBlobs map[string][]byte

func (f fakeBlobs) Get(_ context.Context, key string) (io.ReadCloser, *s3store.Object, error) {
	data, ok := f[key]
	if !ok {
		return nil, nil, s3store.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), &s3store.Object{Key: key, Size: int64(len(data))}, nil
}

// dav is a WebDAV server that remembers what it was given.
type dav struct {
	mu      sync.Mutex
	files   map[string][]byte
	headers map[string]http.Header
	mkcols  []string
	auth    string
}

func newDAV() *dav {
	return &dav{files: map[string][]byte{}, headers: map[string]http.Header{}}
}

func (d *dav) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.auth = r.Header.Get("Authorization")
	switch r.Method {
	case "MKCOL":
		if slices.Contains(d.mkcols, r.URL.Path) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		d.mkcols = append(d.mkcols, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		d.files[r.URL.Path] = body
		d.headers[r.URL.Path] = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	case "PROPFIND":
		data, ok := d.files[r.URL.Path]
		if !ok && slices.Contains(d.mkcols, r.URL.Path) {
			// A folder answers without a size.
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop/><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop><d:getcontentlength>` +
			strconv.Itoa(len(data)) + `</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func seeded(t *testing.T) *tree.Tree {
	t.Helper()
	tr := tree.New(treetest.New(""))
	mtime := time.Unix(1700000000, 0).UTC()
	for _, e := range []tree.Entry{
		{SpaceID: sp, FileID: "f1", Path: "/movies/sub/a.mp4", BlobID: "blob-a", Size: 5, Mime: "video/mp4", MTime: mtime, SHA1: "aaaa"},
		{SpaceID: sp, FileID: "f2", Path: "/root.mp4", BlobID: "blob-r", Size: 5, MTime: mtime},
		{SpaceID: sp, FileID: "f3", Path: "/unknown.mp4", Size: 5},
		{SpaceID: sp, FileID: "f4", Path: "/wrong-size.mp4", BlobID: "blob-w", Size: 99},
	} {
		if err := tr.PutFile(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.PutSpace(context.Background(), tree.Space{ID: sp, Name: "Alan Turing"}); err != nil {
		t.Fatal(err)
	}
	return tr
}

func blobs() fakeBlobs {
	return fakeBlobs{
		tree.BlobKey(sp, "blob-a"): []byte("aaaaa"),
		tree.BlobKey(sp, "blob-r"): []byte("rrrrr"),
		tree.BlobKey(sp, "blob-w"): []byte("w"),
	}
}

func TestRunRestoresIntoWebDAV(t *testing.T) {
	server := newDAV()
	ts := httptest.NewServer(server)
	defer ts.Close()

	sink, err := NewWebDAV(Target{Template: ts.URL + "/dav/{space_name}/", SpaceMap: nil}, Credentials{User: "alan", Password: "token"}, false)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(t.Context(), seeded(t), blobs(), sink, Options{Workers: 2}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if report.Spaces != 1 || report.Restored != 2 || report.Failed != 1 || len(report.NoBlob) != 1 || report.OK() {
		t.Errorf("report = %+v", report)
	}
	if report.NoBlob[0] != sp+"/unknown.mp4" || !strings.Contains(report.Errors[0], "wrong-size.mp4") {
		t.Errorf("no blob %v, errors %v", report.NoBlob, report.Errors)
	}

	// The space name is the folder; the server sees the paths decoded.
	if got := server.files["/dav/Alan Turing/movies/sub/a.mp4"]; string(got) != "aaaaa" {
		t.Errorf("a.mp4 = %q, files %v", got, keys(server.files))
	}
	if got := server.files["/dav/Alan Turing/root.mp4"]; string(got) != "rrrrr" {
		t.Errorf("root.mp4 = %q", got)
	}
	headers := server.headers["/dav/Alan Turing/movies/sub/a.mp4"]
	if headers.Get("X-Oc-Mtime") != "1700000000" || headers.Get("Oc-Checksum") != "SHA1:aaaa" || headers.Get("Content-Type") != "video/mp4" {
		t.Errorf("headers = %v", headers)
	}
	if !strings.HasPrefix(server.auth, "Basic ") {
		t.Errorf("auth = %q", server.auth)
	}
	// The folder of the space and the ones of the path were created top
	// down, each once.
	want := []string{"/dav/Alan Turing", "/dav/Alan Turing/movies", "/dav/Alan Turing/movies/sub"}
	if !slices.Equal(server.mkcols, want) {
		t.Errorf("mkcols = %v", server.mkcols)
	}
}

func TestSkipExistingAndDryRun(t *testing.T) {
	server := newDAV()
	ts := httptest.NewServer(server)
	defer ts.Close()
	sink, _ := NewWebDAV(Target{Template: ts.URL + "/dav/{space_id}"}, Credentials{Bearer: "b"}, false)
	tr := seeded(t)

	first, err := Run(t.Context(), tr, blobs(), sink, Options{}, slog.New(slog.DiscardHandler))
	if err != nil || first.Restored != 2 {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	if server.auth != "Bearer b" {
		t.Errorf("auth = %q", server.auth)
	}

	second, err := Run(t.Context(), tr, blobs(), sink, Options{SkipExisting: true}, slog.New(slog.DiscardHandler))
	if err != nil || second.Restored != 0 || second.Skipped != 2 {
		t.Errorf("second run = %+v, %v", second, err)
	}

	// A dry run lists every file with a blob; the sizes are not checked.
	puts := len(server.files)
	dry, err := Run(t.Context(), tr, blobs(), sink, Options{DryRun: true}, slog.New(slog.DiscardHandler))
	if err != nil || dry.Restored != 3 || len(server.files) != puts {
		t.Errorf("dry run = %+v, %v, files %d", dry, err, len(server.files))
	}
}

func TestFilters(t *testing.T) {
	server := newDAV()
	ts := httptest.NewServer(server)
	defer ts.Close()
	sink, _ := NewWebDAV(Target{Template: ts.URL + "/dav/{space_id}"}, Credentials{User: "u"}, false)

	report, err := Run(t.Context(), seeded(t), blobs(), sink, Options{Prefix: "/movies"}, slog.New(slog.DiscardHandler))
	if err != nil || report.Restored != 1 || len(server.files) != 1 {
		t.Errorf("prefix: %+v, %v", report, err)
	}
	report, err = Run(t.Context(), seeded(t), blobs(), sink, Options{Spaces: []string{"other"}}, slog.New(slog.DiscardHandler))
	if err != nil || report.Spaces != 0 {
		t.Errorf("space filter: %+v, %v", report, err)
	}
}

func TestTargetResolve(t *testing.T) {
	target := Target{Template: "https://cloud/remote.php/dav/spaces/{space_id}/", SpaceMap: map[string]string{"old": "st$new"}}
	if got := target.Resolve(spaceInfo{ID: "old", Name: "x"}); got != "https://cloud/remote.php/dav/spaces/st$new" {
		t.Errorf("mapped = %q", got)
	}
	if got := target.Resolve(spaceInfo{ID: "other", Name: "x"}); got != "https://cloud/remote.php/dav/spaces/other" {
		t.Errorf("unmapped = %q", got)
	}
	if got := escape("/a b/c#d.mp4"); got != "/a%20b/c%23d.mp4" {
		t.Errorf("escape = %q", got)
	}
	if _, err := NewWebDAV(Target{Template: "ftp://x"}, Credentials{}, false); err == nil {
		t.Error("an ftp target was accepted")
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

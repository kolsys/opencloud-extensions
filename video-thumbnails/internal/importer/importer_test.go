package importer

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/render"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

const sp = "space-1"

var root = cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: sp}

type fakePlatform struct {
	files map[string]*cs3.ResourceInfo
}

func (f *fakePlatform) ListSpaces(context.Context) ([]cs3.Space, error) {
	return []cs3.Space{
		{ID: "st$" + sp, Root: root, Name: "Creatives", Type: "project"},
		{ID: "st$twin-a", Root: cs3.Ref{SpaceID: "twin-a", OpaqueID: "twin-a"}, Name: "Twin", Type: "project"},
		{ID: "st$twin-b", Root: cs3.Ref{SpaceID: "twin-b", OpaqueID: "twin-b"}, Name: "Twin", Type: "project"},
	}, nil
}

func (f *fakePlatform) Stat(_ context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error) {
	info, ok := f.files[ref.Path]
	if !ok {
		return nil, cs3.ErrNotFound
	}
	return info, nil
}

type fakeMasters struct {
	mu      sync.Mutex
	masters map[string]store.Master
	frames  map[string][]byte
}

func (m *fakeMasters) HeadMaster(_ context.Context, spaceID, fileID string) (*store.Master, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	master, ok := m.masters[spaceID+"/"+fileID]
	if !ok {
		return nil, s3store.ErrNotFound
	}
	return &master, nil
}

func (m *fakeMasters) PutMaster(_ context.Context, spaceID, fileID string, jpeg []byte, master store.Master) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.masters[spaceID+"/"+fileID] = master
	m.frames[spaceID+"/"+fileID] = jpeg
	return nil
}

func info(id, name, mime, etag string, size uint64) *cs3.ResourceInfo {
	return &cs3.ResourceInfo{ID: cs3.Ref{StorageID: "st", SpaceID: sp, OpaqueID: id}, Name: name, MimeType: mime, ETag: `"` + etag + `"`, Size: size}
}

func pngOf(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, h/2, color.RGBA{G: 200, A: 255})
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// rowsOf hands rows out the way a manifest does.
func rowsOf(rows ...Row) iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		for _, row := range rows {
			if !yield(row, nil) {
				return
			}
		}
	}
}

// collect drains a sequence of rows up to its error.
func collect(rows iter.Seq2[Row, error]) ([]Row, error) {
	var out []Row
	for row, err := range rows {
		if err != nil {
			return out, err
		}
		out = append(out, row)
	}
	return out, nil
}

func TestManifestRows(t *testing.T) {
	m, err := OpenManifest(strings.NewReader("id,Path , thumb\n1,/movies/a.mp4,https://x/a.jpg\n2, b.mp4 ,https://x/b.jpg\n"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collect(m.Rows())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Path != "/movies/a.mp4" || rows[0].Thumb != "https://x/a.jpg" || rows[1].Path != "b.mp4" {
		t.Errorf("rows = %+v", rows)
	}

	// A header without the columns is refused before any row.
	if _, err := OpenManifest(strings.NewReader("path,url\n/a.mp4,https://x\n")); err == nil {
		t.Error("a header without thumb was accepted")
	}

	// A malformed row ends the rows with an error, after the ones before it.
	for name, manifest := range map[string]string{
		"empty path": "path,thumb\n/ok.mp4,https://x\n,https://x\n",
		"short row":  "path,thumb\n/ok.mp4,https://x\n/a.mp4\n",
	} {
		m, err := OpenManifest(strings.NewReader(manifest))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := collect(m.Rows())
		if err == nil || !strings.Contains(err.Error(), "line 3") || len(rows) != 1 || rows[0].Path != "/ok.mp4" {
			t.Errorf("%s: rows %+v, %v", name, rows, err)
		}
	}
}

func TestRunImportsThumbnailsAsMasters(t *testing.T) {
	thumbs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big.png":
			_, _ = w.Write(pngOf(2000, 3000))
		case "/small.png":
			_, _ = w.Write(pngOf(300, 200))
		case "/garbage":
			_, _ = w.Write([]byte("not an image"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer thumbs.Close()

	platform := &fakePlatform{files: map[string]*cs3.ResourceInfo{
		"./movies/tall.mp4": info("v-tall", "tall.mp4", "video/mp4", "e1", 1000),
		"./small.mp4":       info("v-small", "small.mp4", "video/mp4", "e2", 2000),
		"./done.mp4":        info("v-done", "done.mp4", "video/mp4", "e3", 3000),
		"./pic.jpg":         info("pic", "pic.jpg", "image/jpeg", "e4", 10),
		"./broken.mp4":      info("v-broken", "broken.mp4", "video/mp4", "e5", 10),
		"./missing.mp4":     info("v-404", "missing.mp4", "video/mp4", "e6", 10),
	}}
	masters := &fakeMasters{masters: map[string]store.Master{sp + "/v-done": {ETag: "e3"}}, frames: map[string][]byte{}}
	rows := []Row{
		{Path: "/movies/tall.mp4", Thumb: thumbs.URL + "/big.png"},
		{Path: "small.mp4", Thumb: thumbs.URL + "/small.png"},
		{Path: "/done.mp4", Thumb: thumbs.URL + "/big.png"},
		{Path: "/pic.jpg", Thumb: thumbs.URL + "/big.png"},
		{Path: "/broken.mp4", Thumb: thumbs.URL + "/garbage"},
		{Path: "/missing.mp4", Thumb: thumbs.URL + "/nowhere.png"},
		{Path: "/not-there.mp4", Thumb: thumbs.URL + "/big.png"},
	}

	var mu sync.Mutex
	var notImported []string
	opts := Options{Space: "Creatives", MasterSize: 1280, Workers: 3, NotImported: func(path, reason string) {
		mu.Lock()
		defer mu.Unlock()
		notImported = append(notImported, path+": "+reason)
	}}
	report, err := Run(t.Context(), platform, masters, thumbs.Client(), video.NewMatcher([]string{"mp4"}), rowsOf(rows...), opts, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if report.Rows != 7 || report.Imported != 2 || report.Skipped != 1 || report.NotFound != 1 || report.NotVideo != 1 || report.Failed != 2 || report.OK() {
		t.Errorf("report = %+v", report)
	}
	if len(notImported) != 4 {
		t.Errorf("not imported = %v", notImported)
	}

	// The tall thumbnail is fit into the master size, the small one kept.
	tall := masters.masters[sp+"/v-tall"]
	if tall.ETag != "e1" || tall.Mime != "video/mp4" || tall.SourceSize != 1000 {
		t.Errorf("tall master = %+v", tall)
	}
	if size, err := render.Size(masters.frames[sp+"/v-tall"]); err != nil || size != image.Pt(853, 1280) {
		t.Errorf("tall frame = %v, %v", size, err)
	}
	if size, err := render.Size(masters.frames[sp+"/v-small"]); err != nil || size != image.Pt(300, 200) {
		t.Errorf("small frame = %v, %v", size, err)
	}
	if _, ok := masters.frames[sp+"/v-done"]; ok {
		t.Error("a current master was replaced")
	}

	// The same space by id, a dry run fetches nothing.
	fresh := &fakeMasters{masters: map[string]store.Master{}, frames: map[string][]byte{}}
	report, err = Run(t.Context(), platform, fresh, thumbs.Client(), video.NewMatcher([]string{"mp4"}), rowsOf(rows[:2]...), Options{Space: sp, DryRun: true}, slog.New(slog.DiscardHandler))
	if err != nil || report.Imported != 2 || len(fresh.frames) != 0 {
		t.Errorf("dry run = %+v, %v, frames %d", report, err, len(fresh.frames))
	}

	// A manifest that breaks stops the run with what went through before.
	broken := func(yield func(Row, error) bool) {
		if yield(rows[1], nil) {
			yield(Row{}, errors.New("line 3: torn"))
		}
	}
	report, err = Run(t.Context(), platform, fresh, thumbs.Client(), video.NewMatcher([]string{"mp4"}), broken, Options{Space: sp, DryRun: true}, slog.New(slog.DiscardHandler))
	if err == nil || report == nil || report.Rows != 1 || report.Imported != 1 {
		t.Errorf("broken manifest = %+v, %v", report, err)
	}

	// A name two spaces share is refused, an unknown one too.
	if _, err := Run(t.Context(), platform, fresh, thumbs.Client(), video.NewMatcher(nil), rowsOf(), Options{Space: "Twin"}, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("an ambiguous space name was accepted")
	}
	if _, err := Run(t.Context(), platform, fresh, thumbs.Client(), video.NewMatcher(nil), rowsOf(), Options{Space: "nowhere"}, slog.New(slog.DiscardHandler)); err == nil {
		t.Error("an unknown space was accepted")
	}
}

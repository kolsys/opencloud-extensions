package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kolsys/opencloud-extensions/common/cs3"
)

// fakeDownloader serves a file held in memory the way the data server does,
// honouring ranges when told to.
type fakeDownloader struct {
	content []byte
	ranges  bool
	calls   int
}

func (f *fakeDownloader) Download(_ context.Context, _ cs3.Ref, offset, length int64) (io.ReadCloser, bool, error) {
	f.calls++
	if length <= 0 || !f.ranges {
		return io.NopCloser(bytes.NewReader(f.content)), false, nil
	}
	end := min(offset+length, int64(len(f.content)))
	return io.NopCloser(bytes.NewReader(f.content[offset:end])), true, nil
}

func (f *fakeDownloader) SupportsRange(context.Context, cs3.Ref) (bool, error) {
	return f.ranges, nil
}

func TestParseRange(t *testing.T) {
	for _, tc := range []struct {
		header         string
		offset, length int64
		ranged, ok     bool
	}{
		{"", 0, 100, false, true},
		{"bytes=0-", 0, 100, true, true},
		{"bytes=10-19", 10, 10, true, true},
		{"bytes=90-", 90, 10, true, true},
		{"bytes=95-200", 95, 5, true, true},
		{"bytes=-10", 90, 10, true, true},
		{"bytes=-500", 0, 100, true, true},
		{"bytes=100-", 0, 0, false, false},
		{"bytes=5-3", 0, 0, false, false},
		{"bytes=0-1,5-6", 0, 0, false, false},
		{"items=0-1", 0, 0, false, false},
	} {
		offset, length, ranged, ok := parseRange(tc.header, 100)
		if offset != tc.offset || length != tc.length || ranged != tc.ranged || ok != tc.ok {
			t.Errorf("parseRange(%q) = %d %d %t %t, want %d %d %t %t", tc.header, offset, length, ranged, ok, tc.offset, tc.length, tc.ranged, tc.ok)
		}
	}
}

// ffmpeg reads an mp4 without faststart by asking for its tail first: the
// proxy has to answer exactly the range that was asked.
func TestRangeProxyAnswersRanges(t *testing.T) {
	content := []byte(strings.Repeat("0123456789", 10))
	source := NewSource(&fakeDownloader{content: content, ranges: true}, ModeRange, "", slog.New(slog.DiscardHandler))

	url, cleanup, err := source.Open(t.Context(), cs3.Ref{SpaceID: "s", OpaqueID: "f"}, uint64(len(content)), "clip.mp4")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()
	if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, "/clip.mp4") {
		t.Errorf("url = %q", url)
	}

	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	request.Header.Set("Range", "bytes=90-")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET tail: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusPartialContent {
		t.Errorf("code = %d, want 206", response.StatusCode)
	}
	if got := response.Header.Get("Content-Range"); got != "bytes 90-99/100" {
		t.Errorf("Content-Range = %q", got)
	}
	if string(body) != "0123456789" {
		t.Errorf("body = %q", body)
	}

	whole, err := http.Get(url) //nolint:noctx // a test against a loopback listener
	if err != nil {
		t.Fatalf("GET whole: %v", err)
	}
	defer whole.Body.Close()
	all, _ := io.ReadAll(whole.Body)
	if whole.StatusCode != http.StatusOK || len(all) != len(content) || whole.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("whole file: %d, %d bytes, Accept-Ranges=%q", whole.StatusCode, len(all), whole.Header.Get("Accept-Ranges"))
	}

	request, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	request.Header.Set("Range", "bytes=500-")
	beyond, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET beyond: %v", err)
	}
	beyond.Body.Close()
	if beyond.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("range beyond the end: code = %d, want 416", beyond.StatusCode)
	}
}

func TestSourceDownloadsToATempFile(t *testing.T) {
	content := []byte("not really a video")
	source := NewSource(&fakeDownloader{content: content}, ModeDownload, t.TempDir(), slog.New(slog.DiscardHandler))

	path, cleanup, err := source.Open(t.Context(), cs3.Ref{}, uint64(len(content)), "clip.mov")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !strings.HasSuffix(path, ".mov") {
		t.Errorf("path = %q, want the extension of the file kept for ffmpeg", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("temp file = %q, %v", got, err)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the temp file survived the cleanup")
	}
}

// Auto asks the platform once and sticks to the answer.
func TestSourceAutoResolvesOnce(t *testing.T) {
	downloader := &fakeDownloader{content: []byte("x"), ranges: false}
	source := NewSource(downloader, ModeAuto, t.TempDir(), slog.New(slog.DiscardHandler))

	_, cleanup, err := source.Open(t.Context(), cs3.Ref{}, 1, "a.mp4")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cleanup()
	if source.Mode() != ModeDownload {
		t.Errorf("Mode = %s, want download when the platform ignores ranges", source.Mode())
	}

	_, cleanup, err = source.Open(t.Context(), cs3.Ref{}, 1, "b.mp4")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cleanup()
	if downloader.calls != 2 {
		t.Errorf("%d downloads for two opens, the probe was repeated", downloader.calls)
	}
}

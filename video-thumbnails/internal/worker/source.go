// Package worker renders the master frame of a video: it reads the file from
// the platform, runs ffmpeg over it and stores the result.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
)

// Mode is how a video reaches ffmpeg.
type Mode string

// Modes of the source. Auto probes the data server on the first job.
const (
	ModeAuto     Mode = "auto"
	ModeRange    Mode = "range"
	ModeDownload Mode = "download"
)

// proxyTimeouts of the loopback server ffmpeg reads from.
const (
	proxyReadHeaderTimeout = 10 * time.Second
	proxyIdleTimeout       = 2 * time.Minute
)

// Downloader reads a file of the platform. The gateway client implements it.
type Downloader interface {
	Download(ctx context.Context, ref cs3.Ref, offset, length int64) (io.ReadCloser, bool, error)
	SupportsRange(ctx context.Context, ref cs3.Ref) (bool, error)
}

// Source hands the bytes of a video to ffmpeg, either as a loopback URL that
// answers byte ranges from the platform, or as a temporary file.
//
// The loopback proxy is what lets ffmpeg seek: an mp4 without faststart keeps
// its index at the end, and ffmpeg reads it with a range request before it
// decodes the first frame. It also keeps the tokens of the platform out of
// the command line of ffmpeg.
type Source struct {
	cs3     Downloader
	tempDir string
	log     *slog.Logger

	mu   sync.Mutex
	mode Mode
}

// NewSource returns a source in the given mode.
func NewSource(client Downloader, mode Mode, tempDir string, log *slog.Logger) *Source {
	return &Source{cs3: client, mode: mode, tempDir: tempDir, log: log}
}

// Mode reports the mode in use, ModeAuto until the first job resolved it.
func (s *Source) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// Open returns what ffmpeg reads, a URL or a file path, and a function that
// releases it once ffmpeg is done.
func (s *Source) Open(ctx context.Context, ref cs3.Ref, size uint64, name string) (string, func(), error) {
	mode, err := s.resolve(ctx, ref)
	if err != nil {
		return "", nil, err
	}
	if mode == ModeDownload {
		return s.download(ctx, ref, name)
	}
	return s.serve(ctx, ref, size, name)
}

// resolve settles ModeAuto by asking the data server whether it honours a
// range, once.
func (s *Source) resolve(ctx context.Context, ref cs3.Ref) (Mode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.mode != ModeAuto {
		return s.mode, nil
	}

	supported, err := s.cs3.SupportsRange(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("worker: probe range support: %w", err)
	}
	s.mode = ModeDownload
	if supported {
		s.mode = ModeRange
	}
	s.log.Info("source mode resolved", slog.String("mode", string(s.mode)), slog.Bool("range_supported", supported))
	return s.mode, nil
}

// download copies the whole file into a temporary file.
func (s *Source) download(ctx context.Context, ref cs3.Ref, name string) (string, func(), error) {
	body, _, err := s.cs3.Download(ctx, ref, 0, 0)
	if err != nil {
		return "", nil, err
	}
	defer body.Close()

	file, err := os.CreateTemp(s.tempDir, "video-thumbnails-*"+path.Ext(name))
	if err != nil {
		return "", nil, fmt.Errorf("worker: temp file: %w", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) } //nolint:gosec // G703: the name comes from CreateTemp

	if _, err := io.Copy(file, body); err != nil {
		file.Close()
		cleanup()
		return "", nil, fmt.Errorf("worker: download %s: %w", ref, err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("worker: temp file: %w", err)
	}
	return file.Name(), cleanup, nil
}

// serve starts a loopback server that answers ranges of the file from the
// platform for as long as the job runs.
func (s *Source) serve(ctx context.Context, ref cs3.Ref, size uint64, name string) (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("worker: loopback listener: %w", err)
	}

	server := &http.Server{
		Handler:           &rangeProxy{cs3: s.cs3, ref: ref, size: size, ctx: ctx, log: s.log},
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyIdleTimeout,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("loopback server stopped", slog.Any("error", err))
		}
	}()

	url := "http://" + listener.Addr().String() + "/" + path.Base(name)
	return url, func() { _ = server.Close() }, nil
}

// rangeProxy translates the range requests of ffmpeg into range downloads
// from the platform. It knows the size of the file from the job, which the
// answers need for Content-Range.
type rangeProxy struct {
	cs3  Downloader
	ref  cs3.Ref
	size uint64
	ctx  context.Context //nolint:containedctx // bounds the downloads to the job
	log  *slog.Logger
}

func (p *rangeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	offset, length, ranged, ok := parseRange(r.Header.Get("Range"), p.size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatUint(p.size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, p.size))
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	go func() {
		<-r.Context().Done()
		cancel()
	}()

	body, _, err := p.cs3.Download(ctx, p.ref, offset, length)
	if err != nil {
		p.log.Warn("range download failed", slog.Any("error", err))
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer body.Close()

	if ranged {
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, io.LimitReader(body, length))
}

// parseRange reads a single byte range against the size of the file. Without
// a header the whole file is meant.
func parseRange(header string, size uint64) (offset, length int64, ranged, ok bool) {
	total := int64(size) //nolint:gosec // a file size fits
	if header == "" {
		return 0, total, false, true
	}

	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false, false
	}

	switch first {
	case "":
		// The last N bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		n = min(n, total)
		return total - n, n, true, true
	default:
		start, err := strconv.ParseInt(first, 10, 64)
		if err != nil || start < 0 || start >= total {
			return 0, 0, false, false
		}
		end := total - 1
		if last != "" {
			if end, err = strconv.ParseInt(last, 10, 64); err != nil || end < start {
				return 0, 0, false, false
			}
			end = min(end, total-1)
		}
		return start, end - start + 1, true, true
	}
}

// Package importer takes thumbnails made elsewhere and stores them as the
// masters of the videos they belong to, by path: the way a library moves
// in without rendering every video again.
package importer

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/kovidgoyal/imaging"

	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

// Limits of one thumbnail.
const (
	// maxThumb bounds what is downloaded for one master.
	maxThumb = 64 << 20
	// masterQuality is the JPEG quality of an imported master; ffmpeg
	// renders the generated ones at a comparable one.
	masterQuality = 90
)

// Columns of the manifest.
const (
	columnPath  = "path"
	columnThumb = "thumb"
)

// Row is one line of the manifest: a file of the space and where its
// thumbnail is.
type Row struct {
	Path  string
	Thumb string
}

// Manifest is a CSV with a header naming the columns path and thumb; other
// columns are ignored. The rows are read one at a time, so a manifest of
// any length costs the memory of one row.
type Manifest struct {
	reader          *csv.Reader
	pathAt, thumbAt int
}

// OpenManifest reads the header of a manifest.
func OpenManifest(r io.Reader) (*Manifest, error) {
	reader := csv.NewReader(r)
	reader.TrimLeadingSpace = true
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("importer: manifest: read the header: %w", err)
	}
	pathAt, thumbAt := -1, -1
	for i, name := range header {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case columnPath:
			pathAt = i
		case columnThumb:
			thumbAt = i
		}
	}
	if pathAt < 0 || thumbAt < 0 {
		return nil, fmt.Errorf("importer: manifest: the header must name the columns %s and %s", columnPath, columnThumb)
	}
	return &Manifest{reader: reader, pathAt: pathAt, thumbAt: thumbAt}, nil
}

// Rows reads the rows after the header. A malformed row ends the sequence
// with an error naming its line.
func (m *Manifest) Rows() iter.Seq2[Row, error] {
	return func(yield func(Row, error) bool) {
		for line := 2; ; line++ {
			record, err := m.reader.Read()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				yield(Row{}, fmt.Errorf("importer: manifest line %d: %w", line, err))
				return
			}
			if len(record) <= m.pathAt || len(record) <= m.thumbAt {
				yield(Row{}, fmt.Errorf("importer: manifest line %d: too few columns", line))
				return
			}
			row := Row{Path: strings.TrimSpace(record[m.pathAt]), Thumb: strings.TrimSpace(record[m.thumbAt])}
			if row.Path == "" || row.Thumb == "" {
				yield(Row{}, fmt.Errorf("importer: manifest line %d: empty path or thumb", line))
				return
			}
			if !yield(row, nil) {
				return
			}
		}
	}
}

// Platform is the side of the gateway the import needs.
type Platform interface {
	ListSpaces(ctx context.Context) ([]cs3.Space, error)
	Stat(ctx context.Context, ref cs3.Ref) (*cs3.ResourceInfo, error)
}

// Masters is the bucket side the import needs.
type Masters interface {
	HeadMaster(ctx context.Context, spaceID, fileID string) (*store.Master, error)
	PutMaster(ctx context.Context, spaceID, fileID string, jpeg []byte, master store.Master) error
}

// Options say where the files are and how the masters are made.
type Options struct {
	// Space is the id or the exact name of the space the paths are in.
	Space string
	// MasterSize is the long side an imported thumbnail is fit into; a
	// smaller one is stored as it is.
	MasterSize int
	Workers    int
	// DryRun looks the files up and fetches nothing.
	DryRun bool
	// NotImported is told every row that did not go through and why, as
	// the run comes to it. Nil drops them.
	NotImported func(path, reason string)
}

// Report is what a run did.
type Report struct {
	mu       sync.Mutex
	Rows     int
	Imported int
	// Skipped rows had a master of the current version already.
	Skipped int
	// NotFound rows named a path the space does not hold, NotVideo a file
	// that is not a video.
	NotFound int
	NotVideo int
	// Failed rows could not be fetched, decoded or stored.
	Failed int
}

// OK reports whether every row went through or was there already.
func (r *Report) OK() bool {
	return r.NotFound == 0 && r.NotVideo == 0 && r.Failed == 0
}

func (r *Report) add(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn()
}

// Run imports the thumbnails of the rows into the space. An error among the
// rows stops the run; what went through before it stays.
func Run(ctx context.Context, platform Platform, masters Masters, fetch *http.Client, matcher *video.Matcher, rows iter.Seq2[Row, error], opts Options, log *slog.Logger) (*Report, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	space, err := resolveSpace(ctx, platform, opts.Space)
	if err != nil {
		return nil, err
	}

	report := &Report{}
	jobs := make(chan Row)
	var wg sync.WaitGroup
	for range opts.Workers {
		wg.Go(func() {
			for row := range jobs {
				importRow(ctx, platform, masters, fetch, matcher, space, row, opts, report, log)
			}
		})
	}
	stop := func() {
		close(jobs)
		wg.Wait()
	}
	for row, err := range rows {
		if err != nil {
			stop()
			return report, err
		}
		select {
		case jobs <- row:
			report.Rows++
		case <-ctx.Done():
			stop()
			return report, ctx.Err()
		}
	}
	stop()
	return report, nil
}

// resolveSpace finds the one space named by id or by name.
func resolveSpace(ctx context.Context, platform Platform, name string) (*cs3.Space, error) {
	spaces, err := platform.ListSpaces(ctx)
	if err != nil {
		return nil, err
	}
	var found []cs3.Space
	for _, space := range spaces {
		if space.Type != "personal" && space.Type != "project" {
			continue
		}
		if space.ID == name || space.Root.SpaceID == name || space.Name == name {
			found = append(found, space)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("importer: no space %q", name)
	case 1:
		return &found[0], nil
	default:
		ids := make([]string, 0, len(found))
		for _, space := range found {
			ids = append(ids, space.Root.SpaceID)
		}
		return nil, fmt.Errorf("importer: %d spaces are named %q, name one by id: %s", len(found), name, strings.Join(ids, ", "))
	}
}

func importRow(ctx context.Context, platform Platform, masters Masters, fetch *http.Client, matcher *video.Matcher, space *cs3.Space, row Row, opts Options, report *Report, log *slog.Logger) {
	fail := func(count *int, reason string) {
		report.add(func() {
			*count++
			if opts.NotImported != nil {
				opts.NotImported(row.Path, reason)
			}
		})
	}

	ref := cs3.Ref{StorageID: space.Root.StorageID, SpaceID: space.Root.SpaceID, OpaqueID: space.Root.OpaqueID, Path: "./" + strings.TrimPrefix(row.Path, "/")}
	info, err := platform.Stat(ctx, ref)
	switch {
	case errors.Is(err, cs3.ErrNotFound):
		fail(&report.NotFound, "not found in the space")
		return
	case err != nil:
		fail(&report.Failed, err.Error())
		return
	case info.IsDir || !matcher.Match(info.MimeType, info.Name):
		fail(&report.NotVideo, "not a video: "+info.MimeType)
		return
	}
	etag := strings.Trim(info.ETag, `"`)

	master, err := masters.HeadMaster(ctx, space.Root.SpaceID, info.ID.OpaqueID)
	if err != nil && !errors.Is(err, s3store.ErrNotFound) {
		fail(&report.Failed, err.Error())
		return
	}
	if master.Current(etag) {
		report.add(func() { report.Skipped++ })
		return
	}
	if opts.DryRun {
		log.Info("would import", slog.String("path", row.Path), slog.String("thumb", row.Thumb))
		report.add(func() { report.Imported++ })
		return
	}

	frame, err := download(ctx, fetch, row.Thumb)
	if err != nil {
		fail(&report.Failed, err.Error())
		return
	}
	encoded, err := asMaster(frame, opts.MasterSize)
	if err != nil {
		fail(&report.Failed, err.Error())
		return
	}
	if err := masters.PutMaster(ctx, space.Root.SpaceID, info.ID.OpaqueID, encoded, store.Master{
		ETag:       etag,
		Mime:       info.MimeType,
		SourceSize: int64(min(info.Size, 1<<62)), //nolint:gosec,mnd // a file size fits an int64; the clamp keeps the conversion honest
	}); err != nil {
		fail(&report.Failed, err.Error())
		return
	}
	log.Info("imported", slog.String("path", row.Path), slog.Int("bytes", len(encoded)))
	report.add(func() { report.Imported++ })
}

// download fetches a thumbnail by URL.
func download(ctx context.Context, fetch *http.Client, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("importer: %s: %w", url, err)
	}
	response, err := fetch.Do(request) //nolint:gosec // G704: the URL comes from the manifest the operator wrote
	if err != nil {
		return nil, fmt.Errorf("importer: %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("importer: %s: %s", url, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxThumb+1))
	if err != nil {
		return nil, fmt.Errorf("importer: %s: %w", url, err)
	}
	if len(data) > maxThumb {
		return nil, fmt.Errorf("importer: %s: larger than %d bytes", url, maxThumb)
	}
	return data, nil
}

// asMaster turns a thumbnail of any format into a master: a JPEG no larger
// than the master size along its long side, never scaled up.
func asMaster(frame []byte, masterSize int) ([]byte, error) {
	img, err := imaging.Decode(bytes.NewReader(frame), imaging.AutoOrientation(true))
	if err != nil {
		return nil, fmt.Errorf("importer: decode: %w", err)
	}
	if masterSize > 0 {
		img = imaging.Fit(img, masterSize, masterSize, imaging.Lanczos)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: masterQuality}); err != nil {
		return nil, fmt.Errorf("importer: encode: %w", err)
	}
	return buf.Bytes(), nil
}

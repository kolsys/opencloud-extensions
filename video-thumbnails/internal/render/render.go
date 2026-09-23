// Package render produces the variants of a master frame the way the
// thumbnails service of the platform does: the requested size snaps to the
// configured grid, a source smaller than the request is not scaled up, and
// the processor of the request decides how the frame fills the box. One
// thing differs: a frame that a crop would cut down badly, a vertical video
// in a square or a wide box, is fit into the box over a blurred copy of
// itself instead of losing its top and bottom.
package render

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"strconv"
	"strings"

	"github.com/kovidgoyal/imaging"
)

// Processors of the platform. The default is the one it applies to a JPEG.
const (
	ProcessorFit       = "fit"
	ProcessorResize    = "resize"
	ProcessorFill      = "fill"
	ProcessorThumbnail = "thumbnail"
	DefaultProcessor   = ProcessorThumbnail
)

// Revision of the rendering, part of the cache keys: a change in how a
// variant looks must not be hidden by the variants rendered before it.
const Revision = "2"

// How a frame is fit into a box a crop would cut down badly.
const (
	// pillarboxBelow is the share of the frame a centre crop has to keep for
	// the crop to happen; 4:3 into 16:9 keeps 0.75.
	pillarboxBelow = 0.8
	// blurDivisor sets the blur of the background from the size of the box.
	blurDivisor = 40.0
	// darken is how much darker the background is than the frame, in percent.
	darken = -35
)

// Grid is the set of resolutions a request snaps to, in the order they were
// configured.
type Grid []image.Point

// ParseGrid parses resolutions written as WxH.
func ParseGrid(specs []string) (Grid, error) {
	grid := make(Grid, 0, len(specs))
	for _, spec := range specs {
		w, h, found := strings.Cut(strings.TrimSpace(spec), "x")
		width, errW := strconv.Atoi(w)
		height, errH := strconv.Atoi(h)
		if !found || errW != nil || errH != nil || width < 1 || height < 1 {
			return nil, fmt.Errorf("render: resolution %q: want WxH", spec)
		}
		grid = append(grid, image.Pt(width, height))
	}
	return grid, nil
}

// Snap returns the box a request is rendered into, given the size of the
// source. Sizes are compared along the long side of the source: a source
// smaller than the request is returned as it is, otherwise the smallest
// resolution of the grid that is not smaller than the request, or the last
// one when the request exceeds them all. An empty grid keeps the request.
func (g Grid) Snap(requested, source image.Point) image.Point {
	landscape := source.X > source.Y
	if length(source, landscape) < length(requested, landscape) {
		return source
	}
	if len(g) == 0 {
		return requested
	}

	var match image.Point
	best := -1
	for _, candidate := range g {
		diff := length(candidate, landscape) - length(requested, landscape)
		if diff < 0 {
			continue
		}
		if best < 0 || diff < best {
			best, match = diff, candidate
		}
	}
	if best < 0 {
		return g[len(g)-1]
	}
	return match
}

func length(size image.Point, landscape bool) int {
	if landscape {
		return size.X
	}
	return size.Y
}

// Processor normalises the processor of a request. Anything the platform
// does not know falls back to the default.
func Processor(name string) string {
	switch strings.ToLower(name) {
	case ProcessorFit, ProcessorResize, ProcessorFill, ProcessorThumbnail:
		return strings.ToLower(name)
	default:
		return DefaultProcessor
	}
}

// Size reads the dimensions of an encoded frame without decoding it.
func Size(frame []byte) (image.Point, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return image.Point{}, fmt.Errorf("render: read size: %w", err)
	}
	return image.Pt(cfg.Width, cfg.Height), nil
}

// Variant renders a frame into a box with a processor and encodes it as a
// JPEG at the quality the platform uses.
func Variant(frame []byte, box image.Point, processor string) ([]byte, error) {
	img, err := imaging.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("render: decode: %w", err)
	}

	var out image.Image
	switch Processor(processor) {
	case ProcessorFit:
		out = imaging.Fit(img, box.X, box.Y, imaging.Lanczos)
	case ProcessorResize:
		out = imaging.Resize(img, box.X, box.Y, imaging.Lanczos)
	default:
		// fill and thumbnail: the platform crops both to the centre.
		out = fill(img, box)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, nil); err != nil {
		return nil, fmt.Errorf("render: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// fill makes the frame cover the box: cropped to the centre when little is
// lost, fit over a blurred, darkened copy of itself when a crop would cut
// away too much of it.
func fill(img image.Image, box image.Point) image.Image {
	if Kept(img.Bounds().Size(), box) >= pillarboxBelow {
		return imaging.Fill(img, box.X, box.Y, imaging.Center, imaging.Lanczos)
	}

	background := imaging.Fill(img, box.X, box.Y, imaging.Center, imaging.Lanczos)
	background = imaging.Blur(background, float64(max(box.X, box.Y))/blurDivisor)
	background = imaging.AdjustBrightness(background, darken)
	return imaging.PasteCenter(background, imaging.Fit(img, box.X, box.Y, imaging.Lanczos))
}

// Kept is the share of a frame a centre crop into the box keeps: 1 when the
// aspect ratios match, 0.5625 for a vertical 9:16 frame in a square.
func Kept(frame, box image.Point) float64 {
	frameAspect := float64(frame.X) / float64(frame.Y)
	boxAspect := float64(box.X) / float64(box.Y)
	if frameAspect > boxAspect {
		return boxAspect / frameAspect
	}
	return frameAspect / boxAspect
}

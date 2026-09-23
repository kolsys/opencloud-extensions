package render

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/kovidgoyal/imaging"
)

func TestParseGrid(t *testing.T) {
	grid, err := ParseGrid([]string{"16x16", " 500x280", "280x500"})
	if err != nil {
		t.Fatal(err)
	}
	if len(grid) != 3 || grid[1] != image.Pt(500, 280) {
		t.Errorf("grid = %v", grid)
	}

	for _, bad := range []string{"500", "500x", "x280", "0x10", "ax10", "500X280"} {
		if _, err := ParseGrid([]string{bad}); err == nil {
			t.Errorf("ParseGrid(%q) accepted", bad)
		}
	}
}

// The grid of the platform, the expectations follow its ClosestMatch.
var grid = must(ParseGrid([]string{"16x16", "32x32", "64x64", "128x128", "500x280", "280x500", "1000x560", "560x1000", "1920x1080"}))

func TestSnap(t *testing.T) {
	landscape := image.Pt(1280, 720)
	portrait := image.Pt(720, 1280)

	cases := []struct {
		name              string
		requested, source image.Point
		want              image.Point
	}{
		{"exact", image.Pt(32, 32), landscape, image.Pt(32, 32)},
		{"next bigger", image.Pt(36, 36), landscape, image.Pt(64, 64)},
		{"web tile", image.Pt(448, 448), landscape, image.Pt(500, 280)},
		{"portrait picks by height", image.Pt(448, 448), portrait, image.Pt(280, 500)},
		{"portrait exact", image.Pt(280, 500), portrait, image.Pt(280, 500)},
		{"never upscaled", image.Pt(1920, 1080), landscape, landscape},
		{"bigger than the grid", image.Pt(1500, 1500), image.Pt(4000, 3000), image.Pt(1920, 1080)},
		{"square source measures by height", image.Pt(100, 40), image.Pt(600, 600), image.Pt(64, 64)},
	}
	for _, c := range cases {
		if got := grid.Snap(c.requested, c.source); got != c.want {
			t.Errorf("%s: Snap(%v, %v) = %v, want %v", c.name, c.requested, c.source, got, c.want)
		}
	}

	if got := (Grid{}).Snap(image.Pt(40, 40), landscape); got != image.Pt(40, 40) {
		t.Errorf("empty grid = %v, want the request", got)
	}
}

func TestProcessor(t *testing.T) {
	for in, want := range map[string]string{"": "thumbnail", "FIT": "fit", "fill": "fill", "resize": "resize", "magic": "thumbnail"} {
		if got := Processor(in); got != want {
			t.Errorf("Processor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVariant(t *testing.T) {
	master := frame(t, 1280, 720)

	size, err := Size(master)
	if err != nil || size != image.Pt(1280, 720) {
		t.Fatalf("Size = %v, %v", size, err)
	}

	cases := []struct {
		processor string
		box       image.Point
		want      image.Point
	}{
		{"thumbnail", image.Pt(64, 64), image.Pt(64, 64)},
		{"fill", image.Pt(64, 64), image.Pt(64, 64)},
		{"resize", image.Pt(64, 64), image.Pt(64, 64)},
		{"fit", image.Pt(500, 280), image.Pt(497, 280)},
		{"fit", image.Pt(64, 64), image.Pt(64, 36)},
	}
	for _, c := range cases {
		out, err := Variant(master, c.box, c.processor)
		if err != nil {
			t.Fatalf("%s: %v", c.processor, err)
		}
		got, err := Size(out)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s into %v = %v, want %v", c.processor, c.box, got, c.want)
		}
	}

	if _, err := Variant([]byte("not a jpeg"), image.Pt(1, 1), ""); err == nil {
		t.Error("garbage accepted")
	}
}

func TestKept(t *testing.T) {
	cases := []struct {
		frame, box image.Point
		want       float64
	}{
		{image.Pt(1920, 1080), image.Pt(500, 280), 0.995},
		{image.Pt(1080, 1920), image.Pt(64, 64), 0.5625},
		{image.Pt(1920, 1080), image.Pt(64, 64), 0.5625},
		{image.Pt(1080, 1920), image.Pt(500, 280), 0.315},
		{image.Pt(1440, 1080), image.Pt(500, 280), 0.747},
		{image.Pt(100, 100), image.Pt(64, 64), 1},
	}
	for _, c := range cases {
		if got := Kept(c.frame, c.box); got < c.want-0.001 || got > c.want+0.001 {
			t.Errorf("Kept(%v, %v) = %.3f, want %.3f", c.frame, c.box, got, c.want)
		}
	}
}

// A vertical frame in a square or a wide box is not cut to its middle: it is
// fit into the box, the rest is a blurred and darkened copy of it.
func TestFillKeepsAVerticalFrameWhole(t *testing.T) {
	master := plain(t, 720, 1280, color.RGBA{R: 220, G: 220, B: 220, A: 255})

	for _, box := range []image.Point{image.Pt(64, 64), image.Pt(500, 280)} {
		out, err := Variant(master, box, "thumbnail")
		if err != nil {
			t.Fatal(err)
		}
		img, err := imaging.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatal(err)
		}
		if img.Bounds().Size() != box {
			t.Errorf("%v: size %v", box, img.Bounds().Size())
		}
		centre := luma(img.At(box.X/2, box.Y/2))
		corner := luma(img.At(2, 2))
		if centre < 200 {
			t.Errorf("%v: the frame is not in the centre, luma %d", box, centre)
		}
		if corner > centre-50 {
			t.Errorf("%v: the side is not a darkened copy, luma %d against %d", box, corner, centre)
		}
	}

	// A frame that nearly matches the box is cropped as the platform does.
	wide := plain(t, 1920, 1080, color.RGBA{R: 220, G: 220, B: 220, A: 255})
	out, err := Variant(wide, image.Pt(500, 280), "thumbnail")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := imaging.Decode(bytes.NewReader(out))
	if corner := luma(img.At(2, 2)); corner < 200 {
		t.Errorf("a matching frame was pillarboxed, corner luma %d", corner)
	}
}

func plain(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, imaging.New(w, h, c), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func luma(c color.Color) int {
	r, g, b, _ := c.RGBA()
	return int((299*r + 587*g + 114*b) / 1000 >> 8)
}

func frame(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, h/2, color.RGBA{R: uint8(x), A: 255})
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func must(g Grid, err error) Grid {
	if err != nil {
		panic(err)
	}
	return g
}

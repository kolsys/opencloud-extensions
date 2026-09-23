package worker

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	vterrors "github.com/kolsys/opencloud-extensions/video-thumbnails/internal/errors"
)

// clip renders a short synthetic video with the ffmpeg of the host, or skips
// the test when there is none.
func clip(t *testing.T, seconds int, faststart bool, size string) string {
	t.Helper()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	file := filepath.Join(t.TempDir(), "clip.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=10:duration=" + itoa(seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
	}
	if faststart {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, file)

	if out, err := exec.CommandContext(context.Background(), "ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("render clip: %v: %s", err, out)
	}
	return file
}

func itoa(n int) string {
	return string(rune('0' + n))
}

func testFFmpeg() FFmpeg {
	return FFmpeg{Bin: "ffmpeg", Timeout: 30 * time.Second, Seek: time.Second, MasterSize: 320}
}

func TestFrameRendersAScaledJPEG(t *testing.T) {
	frame, err := testFFmpeg().Frame(t.Context(), clip(t, 3, true, "640x360"))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}

	image, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("the frame is not a JPEG: %v", err)
	}
	if bounds := image.Bounds(); bounds.Dx() != 320 || bounds.Dy() != 180 {
		t.Errorf("frame is %dx%d, want 320x180: the long side scaled to the master size", bounds.Dx(), bounds.Dy())
	}
}

func TestFrameDoesNotUpscaleASmallVideo(t *testing.T) {
	frame, err := testFFmpeg().Frame(t.Context(), clip(t, 2, true, "160x120"))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	image, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("the frame is not a JPEG: %v", err)
	}
	if bounds := image.Bounds(); bounds.Dx() != 160 || bounds.Dy() != 120 {
		t.Errorf("frame is %dx%d, want the original 160x120", bounds.Dx(), bounds.Dy())
	}
}

// A clip shorter than the seek has no frame at the seek position; the first
// frame is taken instead of failing.
func TestFrameFallsBackToTheStartOfAShortClip(t *testing.T) {
	ffmpeg := testFFmpeg()
	ffmpeg.Seek = 5 * time.Second

	frame, err := ffmpeg.Frame(t.Context(), clip(t, 1, true, "320x240"))
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(frame)); err != nil {
		t.Errorf("the frame is not a JPEG: %v", err)
	}
}

func TestFrameOfGarbageFails(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "garbage.mp4")
	if err := os.WriteFile(garbage, bytes.Repeat([]byte{0xde, 0xad}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	_, err := testFFmpeg().Frame(t.Context(), garbage)
	if err == nil {
		t.Fatal("garbage rendered a frame")
	}
	if errors.Is(err, vterrors.ErrNoFrame) {
		t.Logf("reported as no frame: %v", err)
	}
}

func TestCheck(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if err := testFFmpeg().Check(t.Context()); err != nil {
		t.Errorf("Check: %v", err)
	}
	if err := (FFmpeg{Bin: "no-such-ffmpeg"}).Check(t.Context()); err == nil {
		t.Error("Check passed for a missing binary")
	}
}

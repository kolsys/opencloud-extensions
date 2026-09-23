package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	vterrors "github.com/kolsys/opencloud-extensions/video-thumbnails/internal/errors"
)

// Limits of one ffmpeg run.
const (
	ffmpegThreads   = 2
	ffmpegQuality   = "3"
	maxStderr       = 2048
	killGracePeriod = 5 * time.Second
)

// FFmpeg renders one frame of a video into a JPEG.
type FFmpeg struct {
	// Bin is the name or the path of the binary.
	Bin string
	// Timeout bounds one run.
	Timeout time.Duration
	// Seek is the position of the frame. A clip shorter than that is read
	// again from its start.
	Seek time.Duration
	// MasterSize is the long side of the frame in pixels; a smaller video is
	// not scaled up.
	MasterSize int
}

// Check reports whether the binary can be run. It backs the readiness probe.
func (f FFmpeg) Check(ctx context.Context) error {
	if _, err := exec.LookPath(f.Bin); err != nil {
		return fmt.Errorf("worker: ffmpeg: %w", err)
	}
	// The binary is the one the operator configured.
	if err := exec.CommandContext(ctx, f.Bin, "-hide_banner", "-version").Run(); err != nil { //nolint:gosec // G204
		return fmt.Errorf("worker: ffmpeg -version: %w", err)
	}
	return nil
}

// Frame renders the master frame of a source, which is a URL or a file path,
// as a JPEG.
func (f FFmpeg) Frame(ctx context.Context, source string) ([]byte, error) {
	frame, err := f.run(ctx, source, f.Seek)
	if errors.Is(err, vterrors.ErrNoFrame) && f.Seek > 0 {
		// Shorter than the seek: take the first frame instead.
		frame, err = f.run(ctx, source, 0)
	}
	return frame, err
}

func (f FFmpeg) run(ctx context.Context, source string, seek time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	size := strconv.Itoa(f.MasterSize)
	scale := "scale='if(gte(iw,ih),min(iw," + size + "),-2)':'if(gte(iw,ih),-2,min(ih," + size + "))'"

	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-threads", strconv.Itoa(ffmpegThreads),
		"-ss", strconv.FormatFloat(seek.Seconds(), 'f', 3, 64),
		"-i", source,
		"-frames:v", "1",
		"-vf", scale,
		"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", ffmpegQuality,
		"pipe:1",
	}

	// The binary is the one the operator configured; the arguments are built
	// here, the source is a loopback URL or a temporary file of ours.
	cmd := exec.CommandContext(ctx, f.Bin, args...) //nolint:gosec // G204
	cmd.WaitDelay = killGracePeriod
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("worker: ffmpeg: %w after %s", ctx.Err(), f.Timeout)
	case err != nil:
		return nil, fmt.Errorf("worker: ffmpeg: %w: %s", err, tail(stderr.String()))
	case stdout.Len() == 0:
		return nil, fmt.Errorf("%w: %s", vterrors.ErrNoFrame, tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// tail keeps the end of the output of ffmpeg, where the reason is.
func tail(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > maxStderr {
		output = output[len(output)-maxStderr:]
	}
	return output
}

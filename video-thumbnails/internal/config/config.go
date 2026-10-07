// Package config holds the configuration of the video-thumbnails service.
package config

import (
	"fmt"
	"time"

	"github.com/kolsys/opencloud-extensions/common/config"
)

// Name of the service, used for the logs, the metric prefix and the names of
// the stream and the consumer group.
const Name = "video-thumbnails"

// Config is the configuration of the service.
type Config struct {
	Platform config.Platform `json:"platform"`
	S3       S3              `json:"s3"`
	FFmpeg   FFmpeg          `json:"ffmpeg"`
	Cache    Cache           `json:"cache"`

	WebDAVUpstream string `json:"webdav_upstream" env:"PLATFORM_WEBDAV_UPSTREAM" desc:"URL of the webdav service of the platform, where previews of non-video files are proxied."`

	HTTPAddr string `json:"http_addr" env:"VIDEO_THUMBNAILS_HTTP_ADDR" desc:"Address the HTTP server listens on."`
	LogLevel string `json:"log_level" env:"OC_LOG_LEVEL;VIDEO_THUMBNAILS_LOG_LEVEL" desc:"Log level: panic, fatal, error, warn, info, debug, trace."`

	Workers       int `json:"workers" env:"VIDEO_THUMBNAILS_WORKERS" desc:"Number of workers taking jobs from the queue."`
	UrgentWorkers int `json:"urgent_workers" env:"VIDEO_THUMBNAILS_URGENT_WORKERS" desc:"Number of workers reserved for the previews missed by the web."`

	// PlatformGenerations bounds the previews the platform generates at once
	// for the requests passed on to it; its own limit counts cache hits too
	// and stays off.
	PlatformGenerations int `json:"platform_generations" env:"VIDEO_THUMBNAILS_PLATFORM_GENERATIONS" desc:"How many previews the platform generates at once for the requests passed on to it. 0 means no limit."`

	// Source is how the worker reads a video: "range" serves ffmpeg over a
	// loopback proxy that asks the platform for byte ranges, "download" pulls
	// the whole file into a temporary file first, "auto" probes the data
	// server on the first job and picks.
	Source  string `json:"source" env:"VIDEO_THUMBNAILS_SOURCE" desc:"How ffmpeg reads a video: auto, range or download."`
	TempDir string `json:"temp_dir" env:"VIDEO_THUMBNAILS_TEMP_DIR" desc:"Directory for downloaded videos when the source is download. Empty means the temporary directory of the system."`

	MasterSize      int      `json:"master_size" env:"VIDEO_THUMBNAILS_MASTER_SIZE" desc:"Long side of the master frame in pixels."`
	Resolutions     []string `json:"resolutions" env:"VIDEO_THUMBNAILS_RESOLUTIONS" desc:"Grid of thumbnail sizes, WxH, comma separated."`
	VideoExtensions []string `json:"video_extensions" env:"VIDEO_THUMBNAILS_VIDEO_EXTENSIONS" desc:"File extensions treated as video, comma separated."`
}

// S3 is the bucket the thumbnails are stored in.
type S3 struct {
	Endpoint  string        `json:"endpoint" env:"VIDEO_THUMBNAILS_S3_ENDPOINT,required" desc:"URL of the S3 endpoint."`
	Region    string        `json:"region" env:"VIDEO_THUMBNAILS_S3_REGION" desc:"Region of the bucket."`
	Bucket    string        `json:"bucket" env:"VIDEO_THUMBNAILS_S3_BUCKET,required" desc:"Bucket the thumbnails are stored in."`
	Prefix    string        `json:"prefix" env:"VIDEO_THUMBNAILS_S3_PREFIX" desc:"Key prefix inside the bucket."`
	AccessKey string        `json:"access_key" env:"VIDEO_THUMBNAILS_S3_ACCESS_KEY,required" desc:"Access key of the bucket."` //nolint:gosec // an identifier, the secret of the pair is SecretKey
	SecretKey config.Secret `json:"secret_key" env:"VIDEO_THUMBNAILS_S3_SECRET_KEY,required" desc:"Secret key of the bucket."`
}

// FFmpeg is how the master frame is produced.
type FFmpeg struct {
	Bin     string        `json:"bin" env:"VIDEO_THUMBNAILS_FFMPEG_BIN" desc:"Name or path of the ffmpeg binary."`
	Timeout time.Duration `json:"timeout" env:"VIDEO_THUMBNAILS_FFMPEG_TIMEOUT" desc:"Time one ffmpeg run may take."`
	Seek    time.Duration `json:"seek" env:"VIDEO_THUMBNAILS_SEEK" desc:"Position of the frame taken from the video."`
}

// Cache is the local disk cache of the rendered variants. Losing it costs
// nothing but the work to render them again.
type Cache struct {
	Dir   string       `json:"dir" env:"VIDEO_THUMBNAILS_DISK_CACHE_DIR" desc:"Directory of the disk cache."`
	Bytes config.Bytes `json:"bytes" env:"VIDEO_THUMBNAILS_DISK_CACHE_BYTES" desc:"Size of the disk cache, like 2Gi."`
}

// DefaultConfig returns the configuration the service starts with when the
// environment says nothing.
func DefaultConfig() *Config {
	return &Config{
		Platform:       config.DefaultPlatform(),
		WebDAVUpstream: "http://opencloud:9115",
		HTTPAddr:       "0.0.0.0:9200",
		LogLevel:       "info",
		Workers:        4,
		UrgentWorkers:  1,

		PlatformGenerations: 2,
		Source:              "auto",
		MasterSize:          1280,
		Resolutions: []string{
			"16x16", "32x32", "64x64", "128x128", "500x280", "280x500",
			"1000x560", "560x1000", "512x2048", "1080x1920", "1920x1080",
			"2160x3840", "3840x2160", "4320x7680", "7680x4320",
		},
		VideoExtensions: []string{"mp4", "mov", "m4v", "webm", "mkv", "avi"},
		S3: S3{
			Region: "default",
			Prefix: "thumbs/",
		},
		FFmpeg: FFmpeg{
			Bin:     "ffmpeg",
			Timeout: 60 * time.Second,
			Seek:    time.Second,
		},
		Cache: Cache{
			Dir:   "/var/cache/video-thumbnails",
			Bytes: 2 << 30,
		},
	}
}

// Validate reports whether the service can start with this configuration.
func (c *Config) Validate() error {
	if err := c.Platform.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"PLATFORM_WEBDAV_UPSTREAM":     c.WebDAVUpstream,
		"VIDEO_THUMBNAILS_S3_ENDPOINT": c.S3.Endpoint,
	} {
		if err := config.ValidateURL(name, value); err != nil {
			return err
		}
	}
	if c.Workers < 1 {
		return fmt.Errorf("config: VIDEO_THUMBNAILS_WORKERS=%d: must be at least 1", c.Workers)
	}
	if c.UrgentWorkers < 1 {
		return fmt.Errorf("config: VIDEO_THUMBNAILS_URGENT_WORKERS=%d: must be at least 1", c.UrgentWorkers)
	}
	if c.PlatformGenerations < 0 {
		return fmt.Errorf("config: VIDEO_THUMBNAILS_PLATFORM_GENERATIONS=%d: must not be negative", c.PlatformGenerations)
	}
	if c.MasterSize < 1 {
		return fmt.Errorf("config: VIDEO_THUMBNAILS_MASTER_SIZE=%d: must be at least 1", c.MasterSize)
	}
	switch c.Source {
	case "auto", "range", "download":
	default:
		return fmt.Errorf("config: VIDEO_THUMBNAILS_SOURCE=%q: must be auto, range or download", c.Source)
	}
	if c.Cache.Bytes < 1 {
		return fmt.Errorf("config: VIDEO_THUMBNAILS_DISK_CACHE_BYTES=%d: must be positive", c.Cache.Bytes)
	}
	return nil
}

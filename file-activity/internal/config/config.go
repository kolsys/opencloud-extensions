// Package config holds the configuration of the file-activity service.
package config

import (
	"fmt"
	"time"

	"github.com/kolsys/opencloud-extensions/common/config"
)

// Name of the service, used for the logs, the metric prefix and the names of
// the stream and the consumer group.
const Name = "file-activity"

// Config is the configuration of the service.
type Config struct {
	Platform config.Platform `json:"platform"`
	Stream   Stream          `json:"stream"`
	Webhook  Webhook         `json:"webhook"`
	Tree     Tree            `json:"tree"`

	HTTPAddr string `json:"http_addr" env:"FILE_ACTIVITY_HTTP_ADDR" desc:"Address the HTTP server listens on."`
	LogLevel string `json:"log_level" env:"OC_LOG_LEVEL;FILE_ACTIVITY_LOG_LEVEL" desc:"Log level: panic, fatal, error, warn, info, debug, trace."`

	AllowedUsers []string `json:"allowed_users" env:"FILE_ACTIVITY_ALLOWED_USERS" desc:"User names allowed to read the feed, comma separated. Empty means everyone."`
}

// Stream is the JetStream stream the feed is kept in.
type Stream struct {
	Name     string        `json:"name" env:"FILE_ACTIVITY_STREAM" desc:"Name of the stream of the feed."`
	Subject  string        `json:"subject" env:"FILE_ACTIVITY_SUBJECT" desc:"Subject the feed is published to."`
	MaxAge   time.Duration `json:"max_age" env:"FILE_ACTIVITY_MAX_AGE" desc:"How long an event stays readable."`
	MaxBytes config.Bytes  `json:"max_bytes" env:"FILE_ACTIVITY_MAX_BYTES" desc:"Size limit of the stream, like 10Gi."`
}

// Tree is the bucket a copy of the tree of the platform is kept in: which
// blob a file is, at which path, in which space, the one thing the platform
// keeps in its metadata only. Without an endpoint no copy is kept.
type Tree struct {
	Endpoint  string        `json:"endpoint" env:"FILE_ACTIVITY_TREE_S3_ENDPOINT" desc:"URL of the S3 endpoint. Empty disables the tree."`
	Region    string        `json:"region" env:"FILE_ACTIVITY_TREE_S3_REGION" desc:"Region of the bucket."`
	Bucket    string        `json:"bucket" env:"FILE_ACTIVITY_TREE_S3_BUCKET" desc:"Bucket the tree is kept in."`
	Prefix    string        `json:"prefix" env:"FILE_ACTIVITY_TREE_S3_PREFIX" desc:"Key prefix inside the bucket."`
	AccessKey string        `json:"access_key" env:"FILE_ACTIVITY_TREE_S3_ACCESS_KEY" desc:"Access key of the bucket."` //nolint:gosec // an identifier, the secret of the pair is SecretKey
	SecretKey config.Secret `json:"secret_key" env:"FILE_ACTIVITY_TREE_S3_SECRET_KEY" desc:"Secret key of the bucket."`
}

// Enabled reports whether a tree is kept.
func (t Tree) Enabled() bool {
	return t.Endpoint != ""
}

// Webhook pushes every event of the feed to an external endpoint.
type Webhook struct {
	URL    string        `json:"url" env:"FILE_ACTIVITY_WEBHOOK_URL" desc:"Endpoint the events are posted to. Empty disables the push."`
	Secret config.Secret `json:"secret" env:"FILE_ACTIVITY_WEBHOOK_SECRET" desc:"Key the HMAC-SHA256 signature of the body is made with."`
}

// Enabled reports whether the push is configured.
func (w Webhook) Enabled() bool {
	return w.URL != ""
}

// DefaultConfig returns the configuration the service starts with when the
// environment says nothing.
func DefaultConfig() *Config {
	return &Config{
		Platform: config.DefaultPlatform(),
		HTTPAddr: "0.0.0.0:9201",
		LogLevel: "info",
		Stream: Stream{
			Name:     Name,
			Subject:  Name + ".events",
			MaxAge:   90 * 24 * time.Hour,
			MaxBytes: 10 << 30,
		},
		Tree: Tree{Region: "default"},
	}
}

// Validate reports whether the service can start with this configuration.
func (c *Config) Validate() error {
	if err := c.Platform.Validate(); err != nil {
		return err
	}
	if c.Tree.Enabled() {
		if err := config.ValidateURL("FILE_ACTIVITY_TREE_S3_ENDPOINT", c.Tree.Endpoint); err != nil {
			return err
		}
		if c.Tree.Bucket == "" {
			return fmt.Errorf("%w: FILE_ACTIVITY_TREE_S3_BUCKET", config.ErrMissing)
		}
	}
	if c.Webhook.Enabled() {
		return config.ValidateURL("FILE_ACTIVITY_WEBHOOK_URL", c.Webhook.URL)
	}
	return nil
}

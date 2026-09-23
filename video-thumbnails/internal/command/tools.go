package command

import (
	"context"
	"flag"
	"log/slog"
	"os"

	commonconfig "github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/config"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
)

// tools is what the commands working on the bucket share: the
// configuration, a logger on stderr, the gateway, the bucket and, when
// asked, the queue.
type tools struct {
	cfg     *config.Config
	log     *slog.Logger
	gateway *cs3.Client
	thumbs  *store.Store
	video   *video.Matcher
	conn    *jsx.Conn
	queue   *queue.Queue
}

// openTools reads the configuration and connects. The caller closes what
// was opened.
func openTools(ctx context.Context, version string, withQueue bool) (*tools, error) {
	cfg := config.DefaultConfig()
	if err := commonconfig.Decode(cfg); err != nil {
		return nil, err
	}

	level, err := obs.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	log := obs.NewLogger(os.Stderr, config.Name, version, level)

	gateway, err := cs3.New(cs3.Config{
		GatewayAddr:          cfg.Platform.GatewayGRPCAddr,
		ServiceAccountID:     cfg.Platform.ServiceAccountID,
		ServiceAccountSecret: cfg.Platform.ServiceAccountSecret.Reveal(),
		Insecure:             cfg.Platform.Insecure,
	}, log)
	if err != nil {
		return nil, err
	}
	t := &tools{cfg: cfg, log: log, gateway: gateway, video: video.NewMatcher(cfg.VideoExtensions)}
	if err := gateway.Ping(ctx); err != nil {
		t.Close()
		return nil, err
	}

	bucket, err := s3store.New(s3store.Config{
		Endpoint:  cfg.S3.Endpoint,
		Region:    cfg.S3.Region,
		Bucket:    cfg.S3.Bucket,
		Prefix:    cfg.S3.Prefix,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
	})
	if err != nil {
		t.Close()
		return nil, err
	}
	if err := bucket.Ping(ctx); err != nil {
		t.Close()
		return nil, err
	}
	t.thumbs = store.New(bucket)

	if withQueue {
		t.conn, err = jsx.Connect(jsx.Config{
			Name:        config.Name + "-resync",
			Endpoint:    cfg.Platform.EventsEndpoint,
			Cluster:     cfg.Platform.EventsCluster,
			EnableTLS:   cfg.Platform.EventsEnableTLS,
			TLSInsecure: cfg.Platform.EventsTLSInsecure,
			Username:    cfg.Platform.EventsAuthUsername,
			Password:    cfg.Platform.EventsAuthPassword,
		}, log)
		if err != nil {
			t.Close()
			return nil, err
		}
		if t.queue, err = queue.Open(ctx, t.conn); err != nil {
			t.Close()
			return nil, err
		}
	}
	return t, nil
}

func (t *tools) Close() {
	if t.conn != nil {
		t.conn.Close()
	}
	if t.gateway != nil {
		_ = t.gateway.Close()
	}
}

// repeatable collects a flag given more than once.
type repeatable []string

func (r *repeatable) String() string { return "" }

func (r *repeatable) Set(value string) error {
	*r = append(*r, value)
	return nil
}

// newFlags returns a flag set that reports its errors on stderr.
func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	return flags
}

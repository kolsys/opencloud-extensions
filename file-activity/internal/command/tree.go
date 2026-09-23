package command

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"

	commonconfig "github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
)

// ErrNoTree reports a command that needs the tree on a service configured
// without one.
var ErrNoTree = errors.New("FILE_ACTIVITY_TREE_S3_ENDPOINT is not set, there is no tree")

// tools is what the commands working on the tree share: the configuration,
// a logger on stderr, the tree and the gateway.
type tools struct {
	cfg     *config.Config
	log     *slog.Logger
	bucket  *s3store.Store
	tree    *tree.Tree
	gateway *cs3.Client
}

// openTools reads the configuration and connects to the tree and, when
// asked, to the gateway. The caller closes what was opened.
func openTools(ctx context.Context, version string, withGateway bool) (*tools, error) {
	cfg := config.DefaultConfig()
	if err := commonconfig.Decode(cfg); err != nil {
		return nil, err
	}
	if !cfg.Tree.Enabled() {
		return nil, ErrNoTree
	}

	level, err := obs.ParseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	log := obs.NewLogger(os.Stderr, config.Name, version, level)

	bucket, err := s3store.New(s3store.Config{
		Endpoint:  cfg.Tree.Endpoint,
		Region:    cfg.Tree.Region,
		Bucket:    cfg.Tree.Bucket,
		Prefix:    cfg.Tree.Prefix,
		AccessKey: cfg.Tree.AccessKey,
		SecretKey: cfg.Tree.SecretKey,
	})
	if err != nil {
		return nil, err
	}
	if err := bucket.Ping(ctx); err != nil {
		return nil, err
	}

	t := &tools{cfg: cfg, log: log, bucket: bucket, tree: tree.New(bucket)}
	if withGateway {
		t.gateway, err = cs3.New(cs3.Config{
			GatewayAddr:          cfg.Platform.GatewayGRPCAddr,
			ServiceAccountID:     cfg.Platform.ServiceAccountID,
			ServiceAccountSecret: cfg.Platform.ServiceAccountSecret.Reveal(),
			Insecure:             cfg.Platform.Insecure,
		}, log)
		if err != nil {
			return nil, err
		}
		if err := t.gateway.Ping(ctx); err != nil {
			t.Close()
			return nil, err
		}
	}
	return t, nil
}

func (t *tools) Close() {
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

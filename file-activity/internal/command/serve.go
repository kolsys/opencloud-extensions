package command

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	commonconfig "github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/cs3"
	"github.com/kolsys/opencloud-extensions/common/httpx"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/common/s3store"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/api"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/feed"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/ingest"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/metrics"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/tree"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/webhook"
)

// startTimeout bounds the calls made before the service is up: the stream
// and the consumers have to exist before anything is served.
const startTimeout = 30 * time.Second

// runServe wires the service together and runs it until the process is
// signalled: the consumer of main-queue, the API, the webhook when one is
// configured, and the observability endpoints.
func runServe(ctx context.Context, version string, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg := config.DefaultConfig()
	if err := commonconfig.Decode(cfg); err != nil {
		return err
	}

	level, err := obs.ParseLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	log := obs.NewLogger(os.Stdout, config.Name, version, level)
	log.Info("starting", slog.Any("config", cfg))

	conn, err := jsx.Connect(jsx.Config{
		Name:        config.Name,
		Endpoint:    cfg.Platform.EventsEndpoint,
		Cluster:     cfg.Platform.EventsCluster,
		EnableTLS:   cfg.Platform.EventsEnableTLS,
		TLSInsecure: cfg.Platform.EventsTLSInsecure,
		Username:    cfg.Platform.EventsAuthUsername,
		Password:    cfg.Platform.EventsAuthPassword,
	}, log)
	if err != nil {
		return err
	}
	defer conn.Close()

	gateway, err := cs3.New(cs3.Config{
		GatewayAddr:          cfg.Platform.GatewayGRPCAddr,
		ServiceAccountID:     cfg.Platform.ServiceAccountID,
		ServiceAccountSecret: cfg.Platform.ServiceAccountSecret.Reveal(),
		Insecure:             cfg.Platform.Insecure,
	}, log)
	if err != nil {
		return err
	}
	defer gateway.Close()

	platform, err := httpx.NewClient(cfg.Platform.InternalURL, cfg.Platform.Insecure)
	if err != nil {
		return err
	}

	var kept ingest.Tree = tree.Disabled{}
	probes := map[string]obs.Probe{"gateway": gateway.Ping}
	if cfg.Tree.Enabled() {
		bucket, err := s3store.New(s3store.Config{
			Endpoint:  cfg.Tree.Endpoint,
			Region:    cfg.Tree.Region,
			Bucket:    cfg.Tree.Bucket,
			Prefix:    cfg.Tree.Prefix,
			AccessKey: cfg.Tree.AccessKey,
			SecretKey: cfg.Tree.SecretKey,
		})
		if err != nil {
			return err
		}
		kept = tree.New(bucket)
		probes["s3"] = bucket.Ping
	} else {
		log.Warn("no tree bucket configured: the feed names blobs on upload only and restore is not possible")
	}

	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()

	store, err := feed.Open(startCtx, conn, cfg.Stream)
	if err != nil {
		return err
	}
	for name, probe := range probes {
		if err := probe(startCtx); err != nil {
			return errors.Join(errors.New("file-activity: "+name+" is not usable"), err)
		}
	}

	registry := obs.NewRegistry(config.Name, version)
	m := metrics.New(registry)

	health := obs.NewHealth()
	health.Register("nats", conn.Ping)
	for name, probe := range probes {
		health.Register(name, probe)
	}

	mux := http.NewServeMux()
	obs.Mount(mux, registry, health)
	api.New(store, platform, cfg.AllowedUsers, m, log).Register(mux)

	tasks := []func(context.Context) error{
		ingest.New(conn, store, ingest.NewMapper(gateway, kept, log), m, config.Name, log).Run,
		obs.NewServer(cfg.HTTPAddr, mux, log).Run,
	}
	if cfg.Webhook.Enabled() {
		tasks = append(tasks, webhook.New(conn, store, cfg.Webhook.URL, cfg.Webhook.Secret,
			config.Name+"-webhook", config.Name+"/"+version, m, log).Run)
	}

	err = runAll(ctx, tasks...)
	log.Info("stopped")
	return err
}

// runAll runs the tasks until one of them fails or the context is cancelled,
// then waits for all of them to return. The first error wins.
func runAll(ctx context.Context, tasks ...func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Go(func() {
			if err := task(ctx); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()

	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

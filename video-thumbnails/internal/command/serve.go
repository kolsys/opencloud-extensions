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
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/cache"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/config"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/events"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/metrics"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/preview"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/queue"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/render"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/store"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/video"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/web"
	"github.com/kolsys/opencloud-extensions/video-thumbnails/internal/worker"
)

// startTimeout bounds the calls made before the service is up.
const startTimeout = 30 * time.Second

// runServe wires the service together and runs it until the process is
// signalled: the reactor on main-queue, the workers of the job queue and the
// HTTP side.
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

	bucket, err := s3store.New(s3store.Config{
		Endpoint:  cfg.S3.Endpoint,
		Region:    cfg.S3.Region,
		Bucket:    cfg.S3.Bucket,
		Prefix:    cfg.S3.Prefix,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
	})
	if err != nil {
		return err
	}
	thumbs := store.New(bucket)

	webdav, err := httpx.NewProxy(cfg.WebDAVUpstream, log)
	if err != nil {
		return err
	}
	platform, err := httpx.NewClient(cfg.Platform.InternalURL, cfg.Platform.Insecure)
	if err != nil {
		return err
	}
	disk, err := cache.Open(cfg.Cache.Dir, int64(cfg.Cache.Bytes))
	if err != nil {
		return err
	}
	grid, err := render.ParseGrid(cfg.Resolutions)
	if err != nil {
		return err
	}
	matcher := video.NewMatcher(cfg.VideoExtensions)

	ffmpeg := worker.FFmpeg{
		Bin:        cfg.FFmpeg.Bin,
		Timeout:    cfg.FFmpeg.Timeout,
		Seek:       cfg.FFmpeg.Seek,
		MasterSize: cfg.MasterSize,
	}

	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()

	jobs, err := queue.Open(startCtx, conn)
	if err != nil {
		return err
	}
	for name, probe := range map[string]obs.Probe{"gateway": gateway.Ping, "s3": thumbs.Ping, "ffmpeg": ffmpeg.Check} {
		if err := probe(startCtx); err != nil {
			return errors.Join(errors.New("video-thumbnails: "+name+" is not usable"), err)
		}
	}

	registry := obs.NewRegistry(config.Name, version)
	m := metrics.New(registry)

	health := obs.NewHealth()
	health.Register("nats", conn.Ping)
	health.Register("gateway", gateway.Ping)
	health.Register("s3", thumbs.Ping)
	health.Register("ffmpeg", ffmpeg.Check)

	webApp, err := web.New(log)
	switch {
	case errors.Is(err, web.ErrNotBuilt):
		log.Warn("web app not built into the binary: the web shows no previews of videos")
	case err != nil:
		return err
	}

	mux := http.NewServeMux()
	obs.Mount(mux, registry, health)
	mux.Handle(web.Route, webApp)
	// Every preview the proxy routes here, of a video or not.
	mux.Handle("/", preview.New(webdav, platform, thumbs, jobs, disk, grid, matcher, m, log))

	source := worker.NewSource(gateway, worker.Mode(cfg.Source), cfg.TempDir, log)

	err = runAll(ctx,
		events.New(conn, gateway, jobs, thumbs, matcher, m, config.Name, log).Run,
		worker.New(jobs, thumbs, gateway, source, ffmpeg, m, cfg.Workers, cfg.UrgentWorkers, log).Run,
		obs.NewServer(cfg.HTTPAddr, mux, log).Run,
	)
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

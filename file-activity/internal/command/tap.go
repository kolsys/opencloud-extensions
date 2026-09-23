package command

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	commonconfig "github.com/kolsys/opencloud-extensions/common/config"
	"github.com/kolsys/opencloud-extensions/common/jsx"
	"github.com/kolsys/opencloud-extensions/common/obs"
	"github.com/kolsys/opencloud-extensions/common/ocevents"
	"github.com/kolsys/opencloud-extensions/file-activity/internal/config"
)

// Permissions of what tap writes: a directory of fixtures and the files in
// it.
const (
	fixtureDirMode  = 0o755
	fixtureFileMode = 0o600
)

// runTap prints the raw envelopes of main-queue and optionally writes them to
// files, which is how the fixtures of the event decoder are taken off a
// stand.
//
// It reads through an ordered consumer, which is ephemeral: the consumer
// groups of the platform and of the extensions are left alone.
func runTap(ctx context.Context, version string, args []string) error {
	flags := flag.NewFlagSet("tap", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dir := flags.String("dir", "", "Write one file per event into this directory.")
	fromStart := flags.Bool("from-start", false, "Start at the first message the stream still holds instead of the next one.")
	timeout := flags.Duration("timeout", time.Minute, "Stop after this long without being interrupted.")
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
	log := obs.NewLogger(os.Stderr, config.Name, version, level)

	conn, err := jsx.Connect(jsx.Config{
		Name:        config.Name + "-tap",
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

	if *dir != "" {
		if err := os.MkdirAll(*dir, fixtureDirMode); err != nil {
			return fmt.Errorf("tap: %w", err)
		}
	}

	deliver := jetstream.DeliverNewPolicy
	if *fromStart {
		deliver = jetstream.DeliverAllPolicy
	}

	consumer, err := conn.JetStream().OrderedConsumer(ctx, ocevents.MainQueue, jetstream.OrderedConsumerConfig{
		DeliverPolicy: deliver,
	})
	if err != nil {
		return fmt.Errorf("tap: %w", err)
	}

	seen := map[string]int{}
	consume, err := consumer.Consume(func(message jetstream.Msg) {
		event, err := ocevents.Decode(message.Data())
		if err != nil {
			log.Error("decode failed", slog.Any("error", err))
			return
		}

		seen[event.Type]++
		fmt.Fprintf(os.Stdout, "%-34s %s known=%t\n", event.Type, event.ID, event.Known())

		if *dir == "" {
			return
		}
		name := fmt.Sprintf("%s-%d.json", event.Type, seen[event.Type])
		if err := os.WriteFile(filepath.Join(*dir, name), message.Data(), fixtureFileMode); err != nil {
			log.Error("write failed", slog.String("file", name), slog.Any("error", err))
		}
	})
	if err != nil {
		return fmt.Errorf("tap: %w", err)
	}
	defer consume.Stop()

	timer := time.NewTimer(*timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
	}

	for eventType, count := range seen {
		fmt.Fprintf(os.Stderr, "%4d %s\n", count, eventType)
	}
	return nil
}

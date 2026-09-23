// Package jsx connects to the NATS of the platform and wraps the JetStream
// calls both extensions need: converging a stream to a configuration,
// durable consumers with explicit acknowledgement, and reading a stream by
// sequence number.
//
// The extensions own their streams and consumer groups. The stream of the
// platform, main-queue, is only consumed: its configuration is never touched.
package jsx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/kolsys/opencloud-extensions/common/config"
)

// Defaults of the connection.
const (
	reconnectWait  = 2 * time.Second
	connectTimeout = 10 * time.Second
	maxReconnects  = -1
)

// Errors returned by this package.
var (
	ErrNoDurableName = errors.New("jsx: a durable consumer needs a name")
	ErrNoEndpoint    = errors.New("jsx: no endpoint to connect to")
)

// Config is how to reach the NATS of the platform. It mirrors the OC_EVENTS_*
// block of the platform.
type Config struct {
	// Name identifies the connection in the output of nats server report.
	Name        string
	Endpoint    string
	Cluster     string
	EnableTLS   bool
	TLSInsecure bool
	Username    string
	Password    config.Secret
}

// Conn is a connection to the NATS of the platform together with its
// JetStream context.
type Conn struct {
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger
}

// Connect opens a connection that keeps reconnecting on its own.
func Connect(cfg Config, log *slog.Logger) (*Conn, error) {
	if cfg.Endpoint == "" {
		return nil, ErrNoEndpoint
	}

	options := []nats.Option{
		nats.Name(cfg.Name),
		nats.MaxReconnects(maxReconnects),
		nats.ReconnectWait(reconnectWait),
		nats.Timeout(connectTimeout),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", slog.Any("error", err))
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", slog.String("url", nc.ConnectedUrl()))
		}),
	}
	if cfg.Username != "" {
		options = append(options, nats.UserInfo(cfg.Username, cfg.Password.Reveal()))
	}
	if cfg.EnableTLS {
		options = append(options, nats.Secure(&tls.Config{
			InsecureSkipVerify: cfg.TLSInsecure, //nolint:gosec // the stand runs on a self signed certificate
			MinVersion:         tls.VersionTLS12,
		}))
	}

	nc, err := nats.Connect(cfg.Endpoint, options...)
	if err != nil {
		return nil, fmt.Errorf("jsx: connect to %s: %w", cfg.Endpoint, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jsx: jetstream: %w", err)
	}

	return &Conn{nc: nc, js: js, log: log}, nil
}

// Close drains the connection and waits for the handlers to return.
func (c *Conn) Close() {
	if err := c.nc.Drain(); err != nil {
		c.log.Warn("nats drain failed", slog.Any("error", err))
		c.nc.Close()
	}
}

// JetStream returns the JetStream context for the calls this package does not
// wrap.
func (c *Conn) JetStream() jetstream.JetStream {
	return c.js
}

// Ping reports whether JetStream answers. It backs the readiness probe.
func (c *Conn) Ping(ctx context.Context) error {
	if !c.nc.IsConnected() {
		return fmt.Errorf("jsx: not connected to %s", c.nc.ConnectedUrl())
	}
	if _, err := c.js.AccountInfo(ctx); err != nil {
		return fmt.Errorf("jsx: account info: %w", err)
	}
	return nil
}

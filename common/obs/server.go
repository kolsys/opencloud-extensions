package obs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Timeouts of the HTTP server. There is no write timeout on purpose: the
// services stream thumbnails and proxy responses of the platform.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second

	// DefaultShutdownTimeout bounds the wait for the open requests to finish.
	DefaultShutdownTimeout = 30 * time.Second
)

// Server runs an HTTP server and shuts it down gracefully.
type Server struct {
	http            *http.Server
	log             *slog.Logger
	ShutdownTimeout time.Duration
}

// NewServer returns a server serving handler on addr.
func NewServer(addr string, handler http.Handler, log *slog.Logger) *Server {
	return &Server{
		http: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
		},
		log:             log,
		ShutdownTimeout: DefaultShutdownTimeout,
	}
}

// Run serves until ctx is cancelled, then waits for the open requests to
// finish. A clean shutdown returns nil.
func (s *Server) Run(ctx context.Context) error {
	errs := make(chan error, 1)
	go func() {
		s.log.Info("http server listening", slog.String("addr", s.http.Addr))
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	s.log.Info("http server shutting down", slog.Duration("timeout", s.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.ShutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errs
}

// Package server builds the HTTPS listener: TLS, auth, routing, and shutdown.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/mmanciop/systemd-unit-healthz/internal/config"
	"github.com/mmanciop/systemd-unit-healthz/internal/telemetry"
)

// Server owns the listener and the HTTP server built around it.
type Server struct {
	cfg    *config.Config
	http   *http.Server
	logger *slog.Logger
}

// New assembles the server. Handler is the health handler; it is wrapped in
// auth and then in instrumentation.
func New(cfg *config.Config, handler http.Handler, tel *telemetry.Telemetry, logger *slog.Logger) (*Server, error) {
	reloader, err := newCertReloader(
		cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.ReloadInterval.Duration(), logger)
	if err != nil {
		return nil, err
	}

	guarded := handler
	if cfg.Auth.Kind == config.AuthHeader {
		auth, err := newAuthenticator(
			cfg.Auth.Header, cfg.Auth.TokenFile, cfg.TLS.ReloadInterval.Duration())
		if err != nil {
			return nil, err
		}
		guarded = auth.wrap(handler, func(r *http.Request, reason string) {
			tel.RecordAuthFailure(r.Context(), reason)
			// No client address and no presented value: one is unbounded
			// cardinality on an internet-facing listener, the other is a secret.
			logger.WarnContext(r.Context(), "auth.rejected", "reason", reason)
		})
	}

	// Instrumentation is outermost so rejected requests are still traced, and
	// scoped to this one route so the 404 handler below costs nothing.
	//
	// http.route comes from http.Request.Pattern, which ServeMux sets to the
	// pattern it matched -- so it is the configured path, never the raw request
	// path a scanner sent. That is what keeps the metric's cardinality bounded,
	// and it is why otelhttp no longer has a WithRouteTag helper.
	instrumented := otelhttp.NewHandler(
		guarded,
		"health",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			// The configured route, never the raw path: this is the templated
			// low-cardinality name, and r.URL.Path on a public listener is
			// whatever a scanner sends.
			return r.Method + " " + cfg.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			if cfg.Auth.Kind != config.AuthHeader {
				return true
			}
			// Drop spans for requests with no credential at all. Internet
			// background noise must not turn into telemetry spend. A wrong
			// token is kept, because that is a real signal.
			return r.Header.Get(cfg.Auth.Header) != ""
		}),
	)

	// A bare mux: anything off the configured path 404s without allocating a
	// span or touching the probe.
	mux := http.NewServeMux()
	mux.Handle(cfg.Path, instrumented)

	return &Server{
		cfg:    cfg,
		logger: logger,
		http: &http.Server{
			Addr:    cfg.Listen,
			Handler: mux,
			TLSConfig: &tls.Config{
				GetCertificate: reloader.GetCertificate,
				MinVersion:     tls.VersionTLS12,
			},
			// This listener faces the internet, so none of these are optional.
			// ReadHeaderTimeout in particular is the Slowloris guard.
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    8 << 10,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
	}, nil
}

// Serve listens and serves until ctx is cancelled, then drains.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		// EACCES on a privileged port is the most likely first-deploy failure
		// and the least self-explanatory, so name the fix.
		if s.cfg.PrivilegedPort() && errors.Is(err, syscall.EACCES) {
			return errors.Join(err, errors.New(
				"binding a port below 1024 needs CAP_NET_BIND_SERVICE; "+
					"check AmbientCapabilities on the systemd unit"))
		}
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("server.listening", "address", s.cfg.Listen, "path", s.cfg.Path)
		// Empty file names: the certificate comes from TLSConfig.GetCertificate.
		if err := s.http.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.logger.Info("server.draining")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.http.Shutdown(shutdownCtx)
}

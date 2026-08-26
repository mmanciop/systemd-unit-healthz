package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves the TLS certificate and picks up renewals.
//
// It re-stats the two files at most once per interval, from inside
// GetCertificate, and reloads when either has changed. Polling rather than
// watching, for three reasons: ACME clients replace the inode on renewal, so a
// watch on the file dies and you have to watch a mode-0750 directory instead
// and debounce the client's own churn; inotify watches fail silently once the
// per-user limit is reached, which is precisely how a previous incarnation of
// this endpoint served a stale self-signed certificate for twenty minutes; and
// an unconditional stat every 30 seconds recovers from any of that on its own.
type certReloader struct {
	certPath, keyPath string
	interval          time.Duration
	logger            *slog.Logger

	mu        sync.Mutex
	current   *tls.Certificate
	stamp     [2]fileStamp
	lastCheck time.Time
}

// fileStamp is the cheap change detector: size plus modification time. A
// content hash would be exact, but this runs on the request path and a
// certificate file that changes without either changing does not occur in
// practice.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func newCertReloader(certPath, keyPath string, interval time.Duration, logger *slog.Logger) (*certReloader, error) {
	r := &certReloader{
		certPath: certPath,
		keyPath:  keyPath,
		interval: interval,
		logger:   logger,
	}
	// Load eagerly, and treat failure as fatal to the caller. Coming up with a
	// listener that cannot complete a handshake is worse than not coming up:
	// paired with Restart=always and StartLimitIntervalSec=0 in the unit, the
	// service simply retries until ACME has produced a certificate.
	if err := r.load(); err != nil {
		return nil, err
	}
	r.lastCheck = time.Now()
	return r, nil
}

// load reads both files and replaces the cached certificate.
func (r *certReloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load TLS key pair (%s, %s): %w", r.certPath, r.keyPath, err)
	}
	r.current = &cert
	r.stamp = r.stampFiles()
	return nil
}

func (r *certReloader) stampFiles() [2]fileStamp {
	var out [2]fileStamp
	for i, p := range [2]string{r.certPath, r.keyPath} {
		if fi, err := os.Stat(p); err == nil {
			out[i] = fileStamp{size: fi.Size(), modTime: fi.ModTime()}
		}
	}
	return out
}

// GetCertificate is the tls.Config hook.
func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.interval > 0 && time.Since(r.lastCheck) >= r.interval {
		r.lastCheck = time.Now()
		if next := r.stampFiles(); next != r.stamp {
			previous := r.current
			if err := r.load(); err != nil {
				// Keep serving the last certificate that worked. A renewal
				// caught mid-write, or a key briefly unreadable, must not take
				// the endpoint down.
				r.current = previous
				r.logger.Error("tls.certificate.reload_failed", "error", err.Error())
			} else {
				r.logger.Info("tls.certificate.reloaded",
					"not_after", r.notAfter(),
					"cert_file", r.certPath)
			}
		}
	}

	return r.current, nil
}

// notAfter reports the loaded leaf certificate's expiry, for the reload log
// line. Empty when the leaf cannot be parsed, which is not worth failing over.
func (r *certReloader) notAfter() string {
	if r.current == nil || r.current.Leaf == nil {
		return ""
	}
	return r.current.Leaf.NotAfter.UTC().Format(time.RFC3339)
}

package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Rejection reasons. Bounded on purpose: these end up as a metric attribute,
// and the presented credential never does.
const (
	ReasonMissingToken = "missing_token"
	ReasonBadToken     = "bad_token"
)

// authenticator checks the shared secret carried in a request header.
//
// The token is read from disk rather than passed in, and re-read on the same
// cadence as the TLS certificate, so rotating it is a file write rather than a
// service restart or a config regeneration.
type authenticator struct {
	header    string
	tokenPath string
	interval  time.Duration

	mu        sync.Mutex
	token     string
	lastRead  time.Time
	lastError error
}

func newAuthenticator(header, tokenPath string, interval time.Duration) (*authenticator, error) {
	a := &authenticator{header: header, tokenPath: tokenPath, interval: interval}
	// Fail at startup rather than 403 every request afterwards: an unreadable
	// token file is a deployment mistake, and a service that answers "forbidden"
	// to everything looks identical to a caller using the wrong secret.
	if err := a.read(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *authenticator) read() error {
	raw, err := os.ReadFile(a.tokenPath)
	if err != nil {
		a.lastError = fmt.Errorf("read auth token: %w", err)
		return a.lastError
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		a.lastError = fmt.Errorf("auth token file %s is empty", a.tokenPath)
		return a.lastError
	}
	a.token, a.lastError, a.lastRead = token, nil, time.Now()
	return nil
}

// expected returns the current token, re-reading it at most once per interval.
// A failed re-read keeps the previous value: the same reasoning as the
// certificate reloader, since a token file caught mid-write must not lock
// everyone out.
func (a *authenticator) expected() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.interval > 0 && time.Since(a.lastRead) >= a.interval {
		previous := a.token
		if err := a.read(); err != nil {
			a.token = previous
			a.lastRead = time.Now()
		}
	}
	return a.token
}

// check reports whether the request carries the right credential, and why not
// when it does not.
func (a *authenticator) check(r *http.Request) (ok bool, reason string) {
	presented := r.Header.Get(a.header)
	if presented == "" {
		return false, ReasonMissingToken
	}
	// Hash both sides before comparing so the comparison is constant time
	// regardless of the two lengths -- ConstantTimeCompare returns early on a
	// length mismatch, which leaks the expected length.
	want := sha256.Sum256([]byte(a.expected()))
	got := sha256.Sum256([]byte(presented))
	if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return false, ReasonBadToken
	}
	return true, ""
}

// onReject is called with the reason for every rejected request.
type onReject func(r *http.Request, reason string)

// wrap returns next guarded by the credential check.
func (a *authenticator) wrap(next http.Handler, reject onReject) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, reason := a.check(r); !ok {
			if reject != nil {
				reject(r, reason)
			}
			// 403 rather than 401: there is no challenge to offer, and it is
			// what the nginx-based predecessor returned, so the synthetic check
			// asserting on status codes needs no change.
			http.Error(w, "forbidden\n", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

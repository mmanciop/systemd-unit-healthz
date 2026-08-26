package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeToken(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func TestNewAuthenticatorRejectsUnusableTokenFiles(t *testing.T) {
	if _, err := newAuthenticator("X-Token", filepath.Join(t.TempDir(), "absent"), time.Minute); err == nil {
		t.Error("newAuthenticator accepted a missing token file; it must fail at startup rather than 403 forever")
	}
	if _, err := newAuthenticator("X-Token", writeToken(t, "   \n"), time.Minute); err == nil {
		t.Error("newAuthenticator accepted an empty token file")
	}
}

func TestCheck(t *testing.T) {
	// Trailing newline on purpose: a token file written by a shell almost always
	// has one, and it must not count as part of the secret.
	auth, err := newAuthenticator("X-Health-Token", writeToken(t, "s3cr3t\n"), time.Minute)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}

	tests := []struct {
		name       string
		header     string
		value      string
		wantOK     bool
		wantReason string
	}{
		{name: "correct token", header: "X-Health-Token", value: "s3cr3t", wantOK: true},
		{name: "no header at all", wantOK: false, wantReason: ReasonMissingToken},
		{name: "empty header", header: "X-Health-Token", value: "", wantOK: false, wantReason: ReasonMissingToken},
		{name: "wrong token", header: "X-Health-Token", value: "nope", wantOK: false, wantReason: ReasonBadToken},
		{name: "right value, wrong header", header: "Authorization", value: "s3cr3t", wantOK: false, wantReason: ReasonMissingToken},
		{name: "prefix of the token", header: "X-Health-Token", value: "s3cr", wantOK: false, wantReason: ReasonBadToken},
		{name: "token plus padding", header: "X-Health-Token", value: "s3cr3tt", wantOK: false, wantReason: ReasonBadToken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			ok, reason := auth.check(r)
			if ok != tc.wantOK {
				t.Fatalf("check() ok = %v, want %v", ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Errorf("check() reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

func TestWrapRejectsWith403AndReportsTheReason(t *testing.T) {
	auth, err := newAuthenticator("X-Health-Token", writeToken(t, "s3cr3t"), time.Minute)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}

	var reasons []string
	reached := false
	handler := auth.wrap(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }),
		func(_ *http.Request, reason string) { reasons = append(reasons, reason) },
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	// 403 rather than 401: there is no challenge to offer, and it is what the
	// nginx predecessor returned.
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if reached {
		t.Error("the wrapped handler ran for an unauthenticated request")
	}
	if len(reasons) != 1 || reasons[0] != ReasonMissingToken {
		t.Errorf("reported reasons = %v, want exactly [%s]", reasons, ReasonMissingToken)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Health-Token", "s3cr3t")
	handler.ServeHTTP(rec, r)
	if !reached {
		t.Error("the wrapped handler did not run for an authenticated request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// Rotation is a file write, so a changed file has to be picked up without a
// restart -- and a token file caught mid-write must not lock everyone out.
func TestExpectedRereadsAndSurvivesAFailedReread(t *testing.T) {
	path := writeToken(t, "first")
	auth, err := newAuthenticator("X-Health-Token", path, time.Nanosecond)
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}

	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if got := auth.expected(); got != "second" {
		t.Errorf("expected() = %q after rotation, want %q", got, "second")
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	if got := auth.expected(); got != "second" {
		t.Errorf("expected() = %q after the file vanished, want the last good value %q", got, "second")
	}
}

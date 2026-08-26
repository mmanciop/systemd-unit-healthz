package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
listen: "127.0.0.1:8443"
tls:
  certFile: /tmp/cert.pem
  keyFile: /tmp/key.pem
auth:
  kind: none
units:
  - minecraft.service
`

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Path != DefaultPath {
		t.Errorf("Path = %q, want the default %q", cfg.Path, DefaultPath)
	}
	if got := cfg.ProbeTimeout.Duration(); got != DefaultProbeTimeout {
		t.Errorf("ProbeTimeout = %s, want the default %s", got, DefaultProbeTimeout)
	}
	if got := cfg.SampleInterval.Duration(); got != DefaultSampleInterval {
		t.Errorf("SampleInterval = %s, want the default %s", got, DefaultSampleInterval)
	}
	if got := cfg.TLS.ReloadInterval.Duration(); got != DefaultReloadInterval {
		t.Errorf("TLS.ReloadInterval = %s, want the default %s", got, DefaultReloadInterval)
	}
	if cfg.Telemetry.Enabled() {
		t.Error("Telemetry.Enabled() = true with no configFile; telemetry must be off by default")
	}
}

// The whole point of strict parsing: a typo must stop the service rather than
// silently leave a feature off.
func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte(minimal + "\nunit: minecraft.service\n"))
	if err == nil {
		t.Fatal("Parse accepted an unknown key; it must be rejected")
	}
	if !strings.Contains(err.Error(), "unit") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

func TestParseDurationStrings(t *testing.T) {
	cfg, err := Parse([]byte(minimal + "\nprobeTimeout: 750ms\nsampleInterval: 1m\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := cfg.ProbeTimeout.Duration(), 750*time.Millisecond; got != want {
		t.Errorf("ProbeTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.SampleInterval.Duration(), time.Minute; got != want {
		t.Errorf("SampleInterval = %s, want %s", got, want)
	}
}

func TestParseRejectsNonDurationAndNegative(t *testing.T) {
	for _, value := range []string{"12", "-5s", "soon"} {
		if _, err := Parse([]byte(minimal + "\nprobeTimeout: " + value + "\n")); err == nil {
			t.Errorf("Parse accepted probeTimeout %q", value)
		}
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		wantErr  bool
		contains string
	}{
		{
			name:     "no units",
			yaml:     "listen: \"127.0.0.1:8443\"\ntls:\n  certFile: /c\n  keyFile: /k\nauth:\n  kind: none\n",
			wantErr:  true,
			contains: "units",
		},
		{
			name:    "unit without a type suffix",
			yaml:    minimal + "\n",
			wantErr: false,
		},
		{
			name:     "unit missing the suffix is rejected",
			yaml:     strings.Replace(minimal, "minecraft.service", "minecraft", 1),
			wantErr:  true,
			contains: "unit type suffix",
		},
		{
			name:     "missing cert",
			yaml:     "listen: \"127.0.0.1:8443\"\ntls:\n  keyFile: /k\nauth:\n  kind: none\nunits: [a.service]\n",
			wantErr:  true,
			contains: "tls.certFile",
		},
		{
			name:     "header auth without a token file",
			yaml:     strings.Replace(minimal, "kind: none", "kind: header\n  header: X-Token", 1),
			wantErr:  true,
			contains: "auth.tokenFile",
		},
		{
			name:     "unknown auth kind",
			yaml:     strings.Replace(minimal, "kind: none", "kind: mtls", 1),
			wantErr:  true,
			contains: "auth.kind",
		},
		{
			name:     "path without a leading slash",
			yaml:     minimal + "\npath: healthz\n",
			wantErr:  true,
			contains: "path",
		},
		{
			name:     "listen without a port",
			yaml:     strings.Replace(minimal, `"127.0.0.1:8443"`, `"127.0.0.1"`, 1),
			wantErr:  true,
			contains: "listen",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.wantErr && err == nil {
				t.Fatalf("Parse succeeded, want an error mentioning %q", tc.contains)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}

// Serving unit state to the internet with no credential should take a
// deliberate opt-in, not an omission.
func TestUnauthenticatedPublicListenerNeedsOptIn(t *testing.T) {
	public := strings.Replace(minimal, `"127.0.0.1:8443"`, `":8443"`, 1)

	if _, err := Parse([]byte(public)); err == nil {
		t.Fatal("Parse accepted auth.kind none on a wildcard listener without allowPublicUnauthenticated")
	}

	if _, err := Parse([]byte(public + "\nallowPublicUnauthenticated: true\n")); err != nil {
		t.Fatalf("Parse rejected an explicit opt-in: %v", err)
	}

	// Loopback needs no opt-in.
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("Parse rejected an unauthenticated loopback listener: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(minimal, `"127.0.0.1:8443"`, `"localhost:8443"`, 1))); err != nil {
		t.Fatalf("Parse rejected an unauthenticated localhost listener: %v", err)
	}
}

func TestPrivilegedPort(t *testing.T) {
	for listen, want := range map[string]bool{
		":443":            true,
		"0.0.0.0:80":      true,
		":8443":           false,
		"127.0.0.1:38443": false,
	} {
		cfg := &Config{Listen: listen}
		if got := cfg.PrivilegedPort(); got != want {
			t.Errorf("PrivilegedPort() for %q = %v, want %v", listen, got, want)
		}
	}
}

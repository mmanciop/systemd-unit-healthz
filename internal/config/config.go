// Package config defines the on-disk configuration and how it is loaded.
//
// Loading is strict: an unknown key is an error rather than a warning, so a
// typo in a hand-written or generated config fails at startup with a message
// naming the key, instead of silently leaving a feature switched off.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults applied to any field the config leaves unset.
const (
	DefaultPath           = "/healthz"
	DefaultListen         = ":8443"
	DefaultReloadInterval = 30 * time.Second
	DefaultProbeTimeout   = 3 * time.Second
	DefaultSampleInterval = 15 * time.Second
)

// Auth kinds.
const (
	AuthHeader = "header"
	AuthNone   = "none"
)

// Config is the whole configuration file.
type Config struct {
	Listen string `yaml:"listen"`
	Path   string `yaml:"path"`

	TLS  TLS  `yaml:"tls"`
	Auth Auth `yaml:"auth"`

	Units []string `yaml:"units"`

	ProbeTimeout   Duration `yaml:"probeTimeout"`
	SampleInterval Duration `yaml:"sampleInterval"`

	Telemetry Telemetry `yaml:"telemetry"`

	// AllowPublicUnauthenticated has to be set explicitly to serve a
	// non-loopback listener with auth.kind "none". Serving unit state to the
	// internet without a credential is a decision worth writing down, not one
	// worth reaching by leaving a field empty.
	AllowPublicUnauthenticated bool `yaml:"allowPublicUnauthenticated"`
}

// TLS points at the certificate and key to serve. Both are read at startup and
// re-read on ReloadInterval, so an ACME renewal is picked up without a restart.
type TLS struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`

	// ReloadInterval is the minimum gap between re-stat checks of the two
	// files. Zero disables reloading entirely.
	ReloadInterval Duration `yaml:"reloadInterval"`
}

// Auth describes how a request proves it is allowed to see unit state.
//
// TokenFile rather than an inline token: this config is rendered into the Nix
// store by the NixOS module, and the store is world-readable.
type Auth struct {
	Kind      string `yaml:"kind"`
	Header    string `yaml:"header"`
	TokenFile string `yaml:"tokenFile"`
}

// Telemetry is deliberately just a pointer to a file. Everything about how
// telemetry behaves lives in that file, in the OpenTelemetry declarative
// configuration schema; an empty ConfigFile means telemetry is off.
type Telemetry struct {
	ConfigFile string `yaml:"configFile"`
}

// Enabled reports whether telemetry should be started at all.
func (t Telemetry) Enabled() bool { return strings.TrimSpace(t.ConfigFile) != "" }

// Duration is a time.Duration that reads from YAML as a Go duration string
// ("30s", "2m"). time.Duration itself would unmarshal from an integer count of
// nanoseconds, which nobody wants to write by hand.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("must be a duration string such as %q: %w", "30s", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if parsed < 0 {
		return fmt.Errorf("must not be negative, got %s", parsed)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Load reads, parses, defaults, and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse turns config bytes into a validated Config.
func Parse(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// The whole point of strict loading: unknown fields are errors.
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Path == "" {
		c.Path = DefaultPath
	}
	if c.Auth.Kind == "" {
		c.Auth.Kind = AuthHeader
	}
	if c.TLS.ReloadInterval == 0 {
		c.TLS.ReloadInterval = Duration(DefaultReloadInterval)
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = Duration(DefaultProbeTimeout)
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = Duration(DefaultSampleInterval)
	}
}

// Validate reports every problem it can find, joined into one error, so a
// misconfiguration is fixed in one pass rather than one restart per mistake.
func (c *Config) Validate() error {
	var errs []error

	if len(c.Units) == 0 {
		errs = append(errs, errors.New("units: at least one systemd unit must be listed"))
	}
	for i, u := range c.Units {
		if strings.TrimSpace(u) == "" {
			errs = append(errs, fmt.Errorf("units[%d]: must not be empty", i))
			continue
		}
		// systemd needs the type suffix to resolve a name; "minecraft" and
		// "minecraft.service" are not interchangeable over D-Bus.
		if !strings.Contains(u, ".") {
			errs = append(errs, fmt.Errorf("units[%d]: %q needs a unit type suffix, e.g. %q", i, u, u+".service"))
		}
	}

	if !strings.HasPrefix(c.Path, "/") {
		errs = append(errs, fmt.Errorf("path: %q must start with %q", c.Path, "/"))
	}

	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("listen: %q is not a host:port address: %w", c.Listen, err))
	}

	if c.TLS.CertFile == "" {
		errs = append(errs, errors.New("tls.certFile: required"))
	}
	if c.TLS.KeyFile == "" {
		errs = append(errs, errors.New("tls.keyFile: required"))
	}

	switch c.Auth.Kind {
	case AuthHeader:
		if c.Auth.Header == "" {
			errs = append(errs, errors.New("auth.header: required when auth.kind is \"header\""))
		}
		if c.Auth.TokenFile == "" {
			errs = append(errs, errors.New("auth.tokenFile: required when auth.kind is \"header\""))
		}
	case AuthNone:
		if !c.listensOnLoopback() && !c.AllowPublicUnauthenticated {
			errs = append(errs, fmt.Errorf(
				"auth.kind %q on non-loopback listen address %q: set allowPublicUnauthenticated to confirm this is intended",
				AuthNone, c.Listen))
		}
	default:
		errs = append(errs, fmt.Errorf("auth.kind: %q is not one of %q, %q", c.Auth.Kind, AuthHeader, AuthNone))
	}

	return errors.Join(errs...)
}

// listensOnLoopback reports whether Listen is bound to a loopback address.
// An empty or wildcard host counts as public: ":8443" accepts connections from
// anywhere, which is exactly the case the auth check exists to catch.
func (c *Config) listensOnLoopback() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// PrivilegedPort reports whether the listen address needs CAP_NET_BIND_SERVICE.
// The NixOS module asks the same question in Nix; this exists so the service can
// say something useful in the log when a bind fails with EACCES.
func (c *Config) PrivilegedPort() bool {
	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	p, err := net.LookupPort("tcp", port)
	if err != nil {
		return false
	}
	return p > 0 && p < 1024
}

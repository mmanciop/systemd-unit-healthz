# systemd-unit-healthz

The `systemd-unit-healthz` program serves the state of systemd units as JSON over HTTPS, so an external health check (like [Dash0's synthetic checks](https://www.dash0.com/synthetic-monitoring)) can poll it.
The HTTP server terminates Transport Layer Security (TLS) itself and requires authentication with a shared secret, so there is no reverse proxy to configure and no socket activation.
The health status of the systemd units is read over the D-Bus system bus.
[OpenTelemetry](https://opentelemetry.io/) instrumentation is opt-in via [declarative configuration](https://github.com/open-telemetry/opentelemetry-configuration).

The responses look as follows:

```
$ curl -s -H 'X-Health-Token: ...' https://example.org/healthz | jq
{
  "healthy": true,
  "units": [
    {
      "name": "minecraft.service",
      "healthy": true,
      "active_state": "active",
      "sub_state": "running",
      "active_since": "2026-08-26T09:12:41Z"
    }
  ]
}
```

200 when every configured unit is `active/running`, 503 otherwise.
The body has the same shape either way, so a check can assert on the status code alone.

## What it reports

A unit is healthy when its `ActiveState` is `active` **and** its `SubState` is `running`.
Both halves matter: a unit with `RemainAfterExit` reports `active` after its process has exited, and `SubState` is what separates a running service from one still in `start-pre`.

The response is the instantaneous truth.
There is no debouncing, no grace period, and no maintenance window.
A unit restarting is reported as down, because it is.
Smoothing belongs in whatever alerts on this, where the thresholds live.

> [!IMPORTANT]
> This reports what systemd knows, which is whether the process is running, not whether it is serving.
> A stuck process that holds its main process ID (PID) looks healthy to systemd, and so it looks healthy to `systemd-unit-healthz`.

The `units` appear in the order they are configured, so that JSONPath assertions index into the array.

A unit that cannot be read at all, because no such unit exists or because D-Bus failed, is reported with `"healthy": false` and an `error` string.
That is distinguishable from a unit that is legitimately stopped.

## Configuration

One YAML file, given by `--config` or `SYSTEMD_UNIT_HEALTHZ_CONFIG`.
Parsing is strict: an unknown key is an error, so a typo stops the service instead of quietly leaving a feature off.

```yaml
listen: ":443"                       # default ":8443"
path: "/healthz"

tls:
  certFile: /var/lib/acme/example.org/fullchain.pem
  keyFile: /var/lib/acme/example.org/key.pem
  reloadInterval: 30s                # 0 disables reloading

auth:
  kind: header                       # header | none
  header: X-Health-Token
  tokenFile: /var/lib/secrets/health-token

units:
  - minecraft.service

probeTimeout: 3s
sampleInterval: 15s                  # background metric sampling; 0 disables

telemetry:
  configFile: /etc/systemd-unit-healthz/otel.yaml   # absent means telemetry off
```

Only paths to secrets belong here, never secret material: the NixOS module renders this file into the Nix store, which is world-readable.

`auth.kind: none` is meant for a loopback listener.
On any other address it needs `allowPublicUnauthenticated: true` as well, so that serving unit state to the internet without a credential is a decision someone wrote down.

### TLS

The certificate and key are re-read at most once per `reloadInterval`, from the request path, and reloaded when either file changes.
A failed reload keeps serving the last good certificate, so a renewal caught mid-write cannot take the endpoint down.

Polling rather than watching, deliberately.
An Automatic Certificate Management Environment (ACME) client replaces the inode on renewal, which kills a watch on the file and forces you to watch the directory instead.
Inotify watches also fail silently once the per-user limit is reached.
An unconditional `stat` every 30 seconds recovers from both on its own.

The initial load is fatal.
A listener that cannot complete a handshake is worse than one that is not up yet, and `Restart=always` plus `StartLimitIntervalSec=0` in the unit means the service retries until the certificate exists.

## Telemetry

Telemetry is contingent on `telemetry.configFile`.
With no file, no OpenTelemetry software development kit (SDK) is installed, the global providers stay the API's no-ops, and everything else works normally.

The file follows the [OpenTelemetry declarative configuration](https://github.com/open-telemetry/opentelemetry-configuration) schema and is parsed with [`otelconf`](https://pkg.go.dev/go.opentelemetry.io/contrib/otelconf):

```yaml
file_format: "1.0-rc.2"
propagator:
  composite_list: "tracecontext,baggage"
resource:
  attributes:
    - name: service.name
      value: systemd-unit-healthz
tracer_provider:
  processors:
    - batch:
        exporter:
          otlp_grpc:
            endpoint: http://127.0.0.1:4317
meter_provider:
  readers:
    - periodic:
        exporter:
          otlp_grpc:
            endpoint: http://127.0.0.1:4317
```

> [!WARNING]
> Configure a propagator.
> The propagator comes from this file, so omitting it means incoming `traceparent` headers are ignored and every span is a disconnected root, while the service otherwise looks perfectly instrumented.
> The service logs `telemetry.propagator_missing` at startup when this happens.

### Metrics

| Metric | Type | Attributes |
|---|---|---|
| `systemd.unit.state` | gauge, `1` | `systemd.unit.name`, `systemd.unit.state` |
| `systemd.unit.active` | gauge, `1` | `systemd.unit.name` |
| `systemd.unit.uptime` | gauge, `s` | `systemd.unit.name` |
| `systemd_unit_healthz.auth.failures` | counter | `error.type` |
| `http.server.request.duration` | histogram, `s` | `http.request.method`, `http.route`, `http.response.status_code` |
| `http.server.active_requests` | up-down counter | — |

The `http.server.*` names and `error.type` come from the [OpenTelemetry semantic conventions](https://opentelemetry.io/docs/specs/semconv/).
The `systemd.*` names are project-specific, because the semantic conventions define nothing for systemd units.

`systemd.unit.state` is a state set: `1` for the unit's current `ActiveState` and `0` for each of the other 5.
The explicit zeros are the point.
A query for `state="active"` returns `0` during an outage rather than going absent, which is the difference between an alert that fires and one that has no data.
`systemd.unit.active` is the same fact as a single 0/1 series, for the common alert.

`systemd.unit.uptime` is seconds since the unit last became active, `0` when it is not active.
A drop in it proves a restart happened even when no sample caught the unit down, which is how you detect a crash loop that a scrape interval steps over.

The two `http.server.*` metrics come from `otelhttp`.
`http.route` is taken from the matched `ServeMux` pattern, never the raw request path, which is what keeps its cardinality bounded on a listener the whole internet can reach.

Metrics are recorded both per request and on the `sampleInterval` timer.
Without the timer they would only exist while something was polling, which makes an absence-based alert meaningless.

### Traces

The request handler emits a server span, and each probe cycle a client span named `org.freedesktop.systemd1.Manager/GetUnitProperties` with `rpc.*` attributes.
Requests carrying no credential at all are filtered out of tracing: internet background noise should not become telemetry spend.
A request with a *wrong* credential is traced, because that is a real signal.

### Logs

Structured JSON on stdout, one object per line, for the journal to collect.
Nothing is exported over the OpenTelemetry Protocol (OTLP), so a crash before the SDK is installed still leaves a record.
Lines carry `trace_id` and `span_id` when there is an active span.

## Running it on NixOS

```nix
{
  inputs.systemd-unit-healthz.url = "github:mmanciop/systemd-unit-healthz";

  # in your NixOS configuration:
  imports = [ inputs.systemd-unit-healthz.nixosModules.default ];

  # The module never touches the firewall, so open the port yourself.
  networking.firewall.allowedTCPPorts = [ 443 ];

  services.systemd-unit-healthz = {
    enable = true;
    extraGroups = [ "acme" ];   # to read an ACME-managed key
    settings = {
      listen = ":443";
      path = "/healthz";
      units = [ "minecraft.service" ];
      tls.certFile = "/var/lib/acme/example.org/fullchain.pem";
      tls.keyFile = "/var/lib/acme/example.org/key.pem";
      auth = {
        kind = "header";
        header = "X-Health-Token";
        tokenFile = "/var/lib/secrets/health-token";
      };
    };
    otelSettings = {
      file_format = "1.0-rc.2";
      propagator.composite_list = "tracecontext,baggage";
      tracer_provider.processors = [
        { batch.exporter.otlp_grpc.endpoint = "http://127.0.0.1:4317"; }
      ];
    };
  };
}
```

The module creates a system user, renders both config files, and runs a hardened unit.
Three details are load-bearing and easy to get wrong by hand:

- **Reading the TLS key** is a group-membership question.
  `security.acme` leaves the per-domain directory `0750 acme:acme` and `key.pem` `0640 acme:acme`, so `extraGroups = [ "acme" ];` is the entire read mechanism, for both the traversal into the directory and the key itself.
- **Binding a port below 1024** needs `CAP_NET_BIND_SERVICE`.
  The module grants it only when `listen` names a privileged port.
- **Opening that port** is yours to do.
  The module deliberately does not write a `networking.firewall` rule, because a health endpoint reachable from the internet should be a line you wrote rather than a side effect of enabling a service.

You do not need a `security.acme` `reloadServices` hook.
The service re-stats the certificate and the key on its own, which is the job the nginx-plus-`reloadServices` pair used to do.

### Making the token readable

The token file has to be readable by the service user, and both halves of that are easy to miss.
`systemd.tmpfiles.rules` adjusts a secret that something else provisioned, without the module ever creating the secret:

```nix
systemd.tmpfiles.rules = [
  "d /var/lib/secrets 0711 root root -"
  "z /var/lib/secrets/health-token 0640 root systemd-unit-healthz -"
];
```

The `z` line is the obvious half: a token written by hand is `0600 root:root`, which the service user cannot read.
`z` adjusts an existing path and never creates one, which is what you want for a secret that lives outside the repository.

> [!IMPORTANT]
> The `d` line is the half that is easy to miss.
> A `/var/lib/secrets` left at `0700 root:root` blocks a non-root service from traversing into the directory at all, no matter what the file inside is set to, and the failure looks like a permission bug on the token rather than on its directory.
> `0711` makes the directory traversable but not listable, and each file inside keeps protecting its own contents.

### Naming the service in telemetry

Give the endpoint its own `service.name` in `otelSettings`, distinct from the units it watches.
The point of a separate identity is telling "the watched service is down" apart from "the probe is down", and reusing the watched unit's name throws exactly that distinction away.

### The rest of the options

`package` overrides the package to run.
`user` and `group` rename the service account, which the module creates only when both are left at their default of `systemd-unit-healthz`.
`logLevel` is `debug`, `info`, `warn`, or `error`, and defaults to `info`.

## Building

Build the package:

```
nix build
```

Run the full gate, which builds, runs the tests, and checks `gofmt`:

```
nix flake check
```

Run the tests alone, which is the fast loop while you edit Go code:

```
go test ./...
```

`vendor/` is committed and the package builds with `vendorHash = null`, so a build needs no network access and there is no hash to keep in sync.
After changing dependencies, run `go mod tidy && go mod vendor` and commit the result.
Continuous integration (CI) fails on a stale tree.

The D-Bus probe compiles on macOS but cannot connect there, so its tests use a fake.
Everything else runs anywhere.

## License

Apache-2.0.

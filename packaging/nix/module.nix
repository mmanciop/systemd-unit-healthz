# NixOS module for systemd-unit-healthz.
#
# The service definition lives here, next to the program, because the hardening
# encodes what the program actually needs: a capability to bind a low port, a
# supplementary group to read a TLS key, and an address family for the D-Bus
# socket. A consumer supplies values and nothing else.
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.systemd-unit-healthz;
  format = pkgs.formats.yaml { };

  otelConfigFile =
    if cfg.otelSettings == null then null else format.generate "otel-config.yaml" cfg.otelSettings;

  # telemetry.configFile is derived rather than configured: rendering
  # otelSettings is what turns telemetry on, so there is no way for the two to
  # disagree.
  settings = cfg.settings // lib.optionalAttrs (otelConfigFile != null) {
    telemetry.configFile = "${otelConfigFile}";
  };

  configFile = format.generate "systemd-unit-healthz.yaml" settings;

  # A port below 1024 needs CAP_NET_BIND_SERVICE. Asking here rather than
  # granting it unconditionally keeps the capability off unprivileged
  # deployments, which are the common case for a loopback listener.
  listenPort = lib.toInt (lib.last (lib.splitString ":" cfg.settings.listen));
  needsBindCapability = listenPort > 0 && listenPort < 1024;
in
{
  options.services.systemd-unit-healthz = {
    enable = lib.mkEnableOption "the systemd-unit-healthz HTTPS endpoint";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The systemd-unit-healthz package to run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "systemd-unit-healthz";
      description = "User the service runs as. Created when it is the default.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "systemd-unit-healthz";
      description = "Group the service runs as. Created when it is the default.";
    };

    extraGroups = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "acme" ];
      description = ''
        Supplementary groups for the service user. This is how the service gets
        read access to a TLS key it does not own: an ACME-managed key is
        typically mode 0640 and owned by the ACME group, so group membership is
        the entire mechanism.
      '';
    };

    settings = lib.mkOption {
      type = lib.types.submodule {
        freeformType = format.type;
        options = {
          listen = lib.mkOption {
            type = lib.types.str;
            default = ":8443";
            description = "Address to serve HTTPS on.";
          };
          units = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [ ];
            example = [ "minecraft.service" ];
            description = "Systemd units to report on, in response order.";
          };
        };
      };
      default = { };
      description = ''
        Contents of the configuration file. Rendered to YAML in the Nix store,
        which is world-readable, so this may name a file holding a secret but
        must never contain the secret itself.
      '';
      example = lib.literalExpression ''
        {
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
        }
      '';
    };

    otelSettings = lib.mkOption {
      type = lib.types.nullOr format.type;
      default = null;
      description = ''
        OpenTelemetry declarative configuration, in the schema at
        https://github.com/open-telemetry/opentelemetry-configuration. Leaving
        this null switches telemetry off entirely: no file is rendered and the
        service installs no SDK.

        Include a propagator. Without one the service exports spans happily and
        silently ignores incoming trace context, so every span becomes a root
        and nothing looks wrong.
      '';
      example = lib.literalExpression ''
        {
          file_format = "1.0-rc.2";
          propagator.composite_list = "tracecontext,baggage";
          resource.attributes = [
            { name = "service.name"; value = "systemd-unit-healthz"; }
          ];
          tracer_provider.processors = [
            { batch.exporter.otlp_grpc.endpoint = "http://127.0.0.1:4317"; }
          ];
          meter_provider.readers = [
            { periodic.exporter.otlp_grpc.endpoint = "http://127.0.0.1:4317"; }
          ];
        }
      '';
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [
        "debug"
        "info"
        "warn"
        "error"
      ];
      default = "info";
      description = "Log level for the service's own JSON logs.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.settings.units != [ ];
        message = "services.systemd-unit-healthz.settings.units must list at least one unit.";
      }
      {
        assertion = (cfg.settings.tls.certFile or "") != "" && (cfg.settings.tls.keyFile or "") != "";
        message = "services.systemd-unit-healthz.settings.tls.certFile and .keyFile are both required.";
      }
      {
        assertion =
          (cfg.settings.auth.kind or "header") != "header" || (cfg.settings.auth.tokenFile or "") != "";
        message = "services.systemd-unit-healthz.settings.auth.tokenFile is required when auth.kind is \"header\".";
      }
      {
        # Catching this here beats catching it at runtime: the module owns
        # telemetry.configFile, and a hand-set value would be silently replaced.
        assertion = !(cfg.otelSettings != null && (cfg.settings.telemetry.configFile or "") != "");
        message =
          "Set either services.systemd-unit-healthz.otelSettings or settings.telemetry.configFile, not both.";
      }
    ];

    users.users = lib.mkIf (cfg.user == "systemd-unit-healthz") {
      systemd-unit-healthz = {
        isSystemUser = true;
        group = cfg.group;
        inherit (cfg) extraGroups;
      };
    };

    users.groups = lib.mkIf (cfg.group == "systemd-unit-healthz") {
      systemd-unit-healthz = { };
    };

    systemd.services.systemd-unit-healthz = {
      description = "HTTPS JSON health endpoint for systemd units";
      wantedBy = [ "multi-user.target" ];
      # dbus.socket: the probe talks to the system bus.
      after = [
        "network-online.target"
        "dbus.socket"
      ];
      wants = [ "network-online.target" ];

      # Without these, editing settings changes the store path while the running
      # process keeps its old configuration until something else restarts it.
      restartTriggers = [ configFile ] ++ lib.optional (otelConfigFile != null) otelConfigFile;

      serviceConfig = {
        ExecStart = lib.concatStringsSep " " [
          "${cfg.package}/bin/systemd-unit-healthz"
          "--config ${configFile}"
          "--log-level ${cfg.logLevel}"
        ];

        User = cfg.user;
        Group = cfg.group;

        Restart = "always";
        RestartSec = "5s";
        # Loading the certificate is fatal at startup on purpose, so that a
        # listener never comes up unable to complete a handshake. Without
        # disabling the start limit, a host whose ACME certificate is not issued
        # yet would park the unit in "failed" instead of retrying until it is.
        StartLimitIntervalSec = 0;
        TimeoutStopSec = "15s";

        AmbientCapabilities = lib.optional needsBindCapability "CAP_NET_BIND_SERVICE";
        CapabilityBoundingSet = lib.optional needsBindCapability "CAP_NET_BIND_SERVICE";

        # AF_UNIX is not optional: without it the D-Bus connection fails with a
        # bare EAFNOSUPPORT and nothing points at this line.
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];

        NoNewPrivileges = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectProc = "invisible";
        ProcSubset = "pid";
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallFilter = [ "@system-service" ];
        SystemCallArchitectures = "native";
        UMask = "0077";

        # Deliberately absent, and each for a reason:
        #   PrivateUsers - uid mapping breaks the system bus connection and
        #     conflicts with the ambient capability.
        #   IPAddressDeny - this listener is meant to be reachable.
        #   DynamicUser - a transient user cannot be used with `sudo -u` to
        #     check whether it can actually read the TLS key, which is the
        #     failure this setup hits most often.
      };
    };
  };
}

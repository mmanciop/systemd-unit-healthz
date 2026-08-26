# The systemd-unit-healthz binary.
#
# Callable with callPackage, so the flake and the overlay share one definition.
# Note that version has no default on purpose: a default here is how a package
# ends up shipping a stale version string when the overlay calls it with no
# arguments while the flake passes the real one.
{
  lib,
  buildGoModule,
  version,
}:

buildGoModule {
  pname = "systemd-unit-healthz";
  inherit version;

  # Only the Go sources, so editing the flake, the module, the README, or CI
  # does not change the derivation's input hash and force a rebuild.
  src = lib.fileset.toSource {
    root = ../..;
    fileset = lib.fileset.unions [
      ../../go.mod
      ../../go.sum
      ../../vendor
      ../../main.go
      ../../internal
    ];
  };

  # null, not a hash: vendor/ is committed, so the build runs with -mod=vendor
  # and needs no network and no fixed-output derivation. That matters because
  # the machines that build this do so during nixos-rebuild, where a transient
  # proxy.golang.org failure would fail a deploy for no good reason.
  vendorHash = null;

  # No cgo: the binary is then static, which keeps it independent of the glibc
  # of whatever nixpkgs the consuming flake happens to pin.
  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  meta = {
    description = "Serves the state of systemd units as JSON over HTTPS";
    homepage = "https://github.com/mmanciop/systemd-unit-healthz";
    license = lib.licenses.asl20;
    mainProgram = "systemd-unit-healthz";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}

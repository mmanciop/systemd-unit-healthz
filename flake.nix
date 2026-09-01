{
  description = "Serves the state of systemd units as JSON over HTTPS, for an external health check to poll";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      # Keep in sync with the release tag. It feeds the -X main.version ldflag,
      # and is passed explicitly to every callPackage below so that the flake
      # and the overlay can never disagree about it.
      version = "0.1.1";
    in
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        systemd-unit-healthz = pkgs.callPackage ./packaging/nix/package.nix { inherit version; };
      in
      {
        packages = {
          default = systemd-unit-healthz;
          inherit systemd-unit-healthz;
        };

        # `nix flake check`. The package build already runs `go test` in its
        # check phase, so the build being green is the test suite being green.
        checks = {
          build = systemd-unit-healthz;

          gofmt = pkgs.runCommand "gofmt-check" { nativeBuildInputs = [ pkgs.go ]; } ''
            cd ${
              nixpkgs.lib.fileset.toSource {
                root = ./.;
                fileset = nixpkgs.lib.fileset.unions [
                  ./main.go
                  ./internal
                ];
              }
            }
            unformatted="$(gofmt -l .)"
            if [ -n "$unformatted" ]; then
              echo "gofmt needed:" >&2
              echo "$unformatted" >&2
              exit 1
            fi
            touch $out
          '';
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gotools
            pkgs.golangci-lint
            pkgs.nixfmt
          ];
        };

        formatter = pkgs.nixfmt;
      }
    )
    // {
      # For consumers that want the package in their own nixpkgs instance:
      #   nixpkgs.overlays = [ systemd-unit-healthz.overlays.default ];
      overlays.default = final: _prev: {
        systemd-unit-healthz = final.callPackage ./packaging/nix/package.nix { inherit version; };
      };

      # For consumers that want the service:
      #   imports = [ systemd-unit-healthz.nixosModules.default ];
      # The package default is wired here rather than in the module, so the
      # module needs no overlay and the consumer can still override it.
      nixosModules.default =
        { lib, pkgs, ... }:
        {
          imports = [ ./packaging/nix/module.nix ];
          services.systemd-unit-healthz.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };
      nixosModules.systemd-unit-healthz = self.nixosModules.default;
    };
}

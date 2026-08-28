#!/usr/bin/env bash
#
# Updates flake.lock to the latest nixpkgs and flake-utils revisions, then
# proves the package still builds against the new pin before leaving it there.
#
# A revision that fails to build is discarded and the previous flake.lock is
# restored, because a broken upstream nixpkgs must never be the thing that
# leaves this repository unbuildable. Idempotent: with nothing newer to pin,
# flake.lock is left untouched.
#
# Run it from the repository root. Requires Nix with flakes enabled. Shared by
# .github/workflows/nix-flake-lock.yml and by anyone updating the pin by hand,
# so neither path can commit a pin it did not build.
set -euo pipefail

if [ ! -f "flake.nix" ]; then
	echo "error: run this from the repository root (cannot find flake.nix)" >&2
	exit 1
fi

if ! command -v nix >/dev/null 2>&1; then
	echo "error: nix is not installed or not on PATH" >&2
	exit 1
fi

validate() {
	nix build --print-build-logs && nix flake check --print-build-logs
}

# The bootstrap case. Until flake.lock exists, every evaluation resolves
# nixos-unstable at whatever HEAD happens to be, so there is no previous pin to
# fall back to and a failure has to be fatal rather than silently reverted.
if [ ! -f "flake.lock" ]; then
	echo "no flake.lock yet; creating the first pin."
	nix flake update

	if validate; then
		echo "flake.lock created and validated."
		exit 0
	fi

	echo "error: the first pin does not build; not leaving a broken flake.lock behind." >&2
	rm -f flake.lock
	exit 1
fi

backup="$(mktemp)"
trap 'rm -f "$backup"' EXIT
cp flake.lock "$backup"

nix flake update

if cmp -s flake.lock "$backup"; then
	echo "flake.lock already up to date."
	exit 0
fi

if validate; then
	echo "flake.lock updated and validated."
else
	echo "::warning::The new nixpkgs or flake-utils revision fails to build; keeping the previous pin." >&2
	cp "$backup" flake.lock
fi

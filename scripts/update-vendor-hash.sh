#!/usr/bin/env bash
# Computes the correct vendorHash for flake.nix.
#
# Usage: ./scripts/update-vendor-hash.sh
#
# This triggers a nix build with a fake hash, captures the expected hash
# from the error output, and prints it. You can then update flake.nix
# with the printed value.
set -euo pipefail

echo "Computing vendor hash (this will intentionally fail once)..."

# Temporarily set a fake hash to force nix to report the real one
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
cp flake.nix "$tmpdir/flake.nix.bak"

sed -i 's|vendorHash = "sha256-[^"]*"|vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="|' flake.nix

hash=$(nix build .#default 2>&1 | grep 'got:' | awk '{print $2}' || true)

# Restore original flake.nix
cp "$tmpdir/flake.nix.bak" flake.nix

if [ -z "$hash" ]; then
  echo "ERROR: Could not extract hash. The current vendorHash may already be correct."
  echo "Try running: nix build .#default"
  exit 1
fi

echo ""
echo "Correct vendorHash:"
echo "  vendorHash = \"$hash\";"

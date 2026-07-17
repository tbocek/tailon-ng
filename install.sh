#!/usr/bin/env bash
set -euo pipefail
# Installs the latest tailon-ng release binary for the current OS/arch.
# Usage: curl -sL https://raw.githubusercontent.com/tbocek/tailon-ng/main/install.sh | bash

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64 | amd64)  arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) echo "error: unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in linux | darwin) ;; *) echo "error: unsupported OS: $os" >&2; exit 1 ;; esac

dir="/usr/local/bin"
[ -w "$dir" ] || { dir="$HOME/.local/bin"; mkdir -p "$dir"; }

echo "Downloading..."
tmp="$(mktemp)"
curl -fsSL "https://github.com/tbocek/tailon-ng/releases/latest/download/tailon-ng-${os}-${arch}" -o "$tmp"
chmod +x "$tmp"
mv "$tmp" "$dir/tailon-ng"
echo "Installed tailon-ng to $dir/tailon-ng"

case ":$PATH:" in *":$dir:"*) ;; *) echo "note: $dir is not on your PATH." ;; esac
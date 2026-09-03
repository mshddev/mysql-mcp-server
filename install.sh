#!/bin/sh
# Installs the latest mysql-mcp-server release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/mshddev/mysql-mcp-server/main/install.sh | sh
#
# Environment overrides:
#   VERSION      tag to install, e.g. v0.0.2 (default: latest release)
#   INSTALL_DIR  where to put the binary (default: /usr/local/bin if writable,
#                otherwise ~/.local/bin)
#
# The script never calls sudo. If you want /usr/local/bin and it isn't
# writable, run it with sudo yourself or pick a different INSTALL_DIR.

set -eu

REPO="mshddev/mysql-mcp-server"
BIN="mysql-mcp-server"

die() {
  echo "install.sh: $*" >&2
  exit 1
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "need '$1' on PATH"
}

need curl
need tar
need uname

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux | darwin) ;;
  *) die "unsupported OS '$os'. Use: go install github.com/$REPO@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture '$arch'. Use: go install github.com/$REPO@latest" ;;
esac

if [ -n "${VERSION:-}" ]; then
  tag=$VERSION
  case "$tag" in v*) ;; *) tag="v$tag" ;; esac
else
  # "tag_name": "v0.0.2" -> v0.0.2. No jq dependency.
  tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$tag" ] || die "could not determine the latest release tag"
fi
version=${tag#v}

archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

if [ -z "${INSTALL_DIR:-}" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $BIN $tag ($os/$arch)..."
curl -fsSL -o "$tmp/$archive" "$base/$archive" ||
  die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" ||
  die "download failed: $base/checksums.txt"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d ' ' -f 1)
[ -n "$expected" ] || die "$archive is not listed in checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | cut -d ' ' -f 1)
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$archive" | cut -d ' ' -f 1)
else
  die "need sha256sum or shasum to verify the download"
fi
[ "$actual" = "$expected" ] || die "checksum mismatch for $archive
  expected: $expected
  actual:   $actual"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"

mkdir -p "$INSTALL_DIR"
[ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable. Re-run with sudo, or set INSTALL_DIR to somewhere you own."
install -m 0755 "$tmp/$BIN" "$INSTALL_DIR/$BIN"

echo "Installed $INSTALL_DIR/$BIN"
"$INSTALL_DIR/$BIN" --version

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    echo
    echo "Note: $INSTALL_DIR is not on your PATH. Add it, for example:"
    echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

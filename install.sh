#!/bin/sh
# Installs container-compose from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/pedrospdc/apple-container-compose/main/install.sh | sh
#
# Environment:
#   VERSION=v1.2.3   install a specific release (default: latest)
#   PREFIX=/dir      install directory (default: /usr/local/bin if writable, else ~/.local/bin)
#   ALIAS=yes|no     create a docker-compose alias without asking (default: ask if docker-compose is missing)
set -eu

REPO="pedrospdc/apple-container-compose"
BIN="container-compose"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || die "container-compose runs on macOS only (Apple container)."
[ "$(uname -m)" = "arm64" ] || die "Apple container requires an Apple silicon Mac."
command -v curl >/dev/null || die "curl is required."

VERSION="${VERSION:-latest}"
if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's|.*/tag/||')
  [ -n "$VERSION" ] && [ "${VERSION#v}" != "$VERSION" ] || die "could not determine the latest release."
fi

if [ -z "${PREFIX:-}" ]; then
  if [ -w /usr/local/bin ]; then PREFIX=/usr/local/bin; else PREFIX="$HOME/.local/bin"; fi
fi

ARCHIVE="${BIN}_${VERSION#v}_darwin_arm64.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

say "Downloading $BIN $VERSION..."
curl -fsSL -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE" || die "download failed: $BASE/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" || die "download failed: checksums.txt"
(cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | shasum -a 256 -c - >/dev/null) || die "checksum mismatch for $ARCHIVE"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" "$BIN"
mkdir -p "$PREFIX"
install -m 0755 "$TMP/$BIN" "$PREFIX/$BIN"
say "Installed $PREFIX/$BIN"

case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) say "Note: $PREFIX is not on your PATH. Add it, e.g.: export PATH=\"$PREFIX:\$PATH\"" ;;
esac

if ! command -v container >/dev/null 2>&1; then
  say "Apple container is not installed. Install it with: brew install container"
  say "(or from https://github.com/apple/container/releases), then run: container system start"
fi

# Offer a docker-compose command for Makefiles and scripts. The alias
# subcommand asks on the terminal (works under `curl | sh`) and does nothing
# when another docker-compose exists.
case "${ALIAS:-ask}" in
  yes) PATH="$PREFIX:$PATH" "$PREFIX/$BIN" alias --yes ;;
  no) ;;
  *) PATH="$PREFIX:$PATH" "$PREFIX/$BIN" alias || true ;;
esac

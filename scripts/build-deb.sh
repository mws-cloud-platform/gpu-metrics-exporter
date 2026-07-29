#!/bin/sh
# build-deb.sh — assemble a .deb for one package using dpkg-deb + a DEBIAN/ template.
#
# Usage:
#   scripts/build-deb.sh <package> [version]
#
# <package>  gpu-metrics-exporter | gpu-metrics-receiver
# [version]  deb version; defaults to $VERSION env, then `git describe --tags --always`.
#
# Expects the linux/amd64 binary at bin/<package> and the unit at
# systemd/<package>.service (produce both with `make docker-build`).
set -eu

PKG="${1:-}"
if [ -z "$PKG" ]; then
	echo "usage: $0 <package> [version]" >&2
	exit 2
fi

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEMPLATE="$ROOT/debian/$PKG"
BINARY="$ROOT/bin/$PKG"
SERVICE="$ROOT/systemd/$PKG.service"

if [ ! -d "$TEMPLATE/DEBIAN" ]; then
	echo "error: no DEBIAN template at $TEMPLATE/DEBIAN" >&2
	exit 1
fi
if [ ! -x "$BINARY" ]; then
	echo "error: binary not found (or not executable): $BINARY" >&2
	echo "       run 'make docker-build' first." >&2
	exit 1
fi
if [ ! -f "$SERVICE" ]; then
	echo "error: service unit not found: $SERVICE" >&2
	exit 1
fi

VERSION="${2:-${VERSION:-}}"
if [ -z "$VERSION" ]; then
	if command -v git >/dev/null 2>&1 && [ -d "$ROOT/.git" ]; then
		VERSION="$(git -C "$ROOT" describe --tags --always 2>/dev/null || true)"
	fi
	VERSION="${VERSION:-0.0.0-dev}"
fi

DIST="$ROOT/dist"
mkdir -p "$DIST"

# Fresh staging tree: DEBIAN/ metadata + the installed file layout.
STAGING="$(mktemp -d "${TMPDIR:-/tmp}/${PKG}.deb.XXXXXX")"
trap 'rm -rf "$STAGING"' EXIT

install -d "$STAGING/DEBIAN" \
           "$STAGING/usr/local/bin" \
           "$STAGING/etc/systemd/system" \
           "$STAGING/usr/share/doc/$PKG"

# Metadata. Substitute the version placeholder and keep scripts executable.
sed "s/__VERSION__/$VERSION/g" "$TEMPLATE/DEBIAN/control" > "$STAGING/DEBIAN/control"
for script in postinst prerm postrm; do
	if [ -f "$TEMPLATE/DEBIAN/$script" ]; then
		install -m 0755 "$TEMPLATE/DEBIAN/$script" "$STAGING/DEBIAN/$script"
	fi
done
chmod 0644 "$STAGING/DEBIAN/control"

# Payload.
install -m 0755 "$BINARY"  "$STAGING/usr/local/bin/$PKG"
install -m 0644 "$SERVICE" "$STAGING/etc/systemd/system/$(basename "$SERVICE")"

# Licenses: ship the Apache 2.0 license, NOTICE, and bundled third-party
# licenses so each .deb is self-contained (Apache 2.0 §4 / NOTICE obligation).
for lic in LICENSE NOTICE THIRD_PARTY_LICENSES.txt; do
	if [ -f "$ROOT/$lic" ]; then
		install -m 0644 "$ROOT/$lic" "$STAGING/usr/share/doc/$PKG/$lic"
	fi
done

OUT="$DIST/${PKG}_${VERSION}_amd64.deb"
# --root-owner-group: files in the deb are owned by root:root regardless of the builder uid.
dpkg-deb --build --root-owner-group "$STAGING" "$OUT"

echo "built $OUT"

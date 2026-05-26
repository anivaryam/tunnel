#!/bin/sh
set -e

REPO="anivaryam/tunnel"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

# Detect OS
OS="$(uname -s)"
case "$OS" in
  Linux*)  OS="linux" ;;
  Darwin*) OS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) OS="windows" ;;
  *) echo "Unsupported OS: $OS" && exit 1 ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" && exit 1 ;;
esac

# Get latest version
VERSION="$(curl -sSf https://api.github.com/repos/${REPO}/releases/latest | grep '"tag_name"' | cut -d'"' -f4)"
if [ -z "$VERSION" ]; then
  echo "Failed to fetch latest version"
  exit 1
fi

# Download
EXT="tar.gz"
BIN="tunnel"
if [ "$OS" = "windows" ]; then
  EXT="zip"
  BIN="tunnel.exe"
fi

ASSET="tunnel_${OS}_${ARCH}.${EXT}"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
SUMS_URL="https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt"
echo "Downloading tunnel ${VERSION} for ${OS}/${ARCH}..."

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

curl -sSfL "$URL" -o "${TMP}/${ASSET}"
curl -sSfL "$SUMS_URL" -o "${TMP}/checksums.txt"

# Verify checksum
if command -v sha256sum >/dev/null 2>&1; then
  EXPECTED="$(grep " ${ASSET}\$" "${TMP}/checksums.txt" | awk '{print $1}')"
  ACTUAL="$(sha256sum "${TMP}/${ASSET}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  EXPECTED="$(grep " ${ASSET}\$" "${TMP}/checksums.txt" | awk '{print $1}')"
  ACTUAL="$(shasum -a 256 "${TMP}/${ASSET}" | awk '{print $1}')"
else
  echo "Warning: no sha256 tool found; skipping checksum verification" >&2
  EXPECTED=""
  ACTUAL=""
fi
if [ -n "$EXPECTED" ] && [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "Checksum mismatch for ${ASSET}" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $ACTUAL" >&2
  exit 1
fi

# Extract
if [ "$EXT" = "zip" ]; then
  unzip -q "${TMP}/${ASSET}" -d "$TMP"
else
  tar -xzf "${TMP}/${ASSET}" -C "$TMP"
fi

# Install
if [ -w "$INSTALL_DIR" ]; then
  mv "${TMP}/${BIN}" "${INSTALL_DIR}/${BIN}"
else
  sudo mv "${TMP}/${BIN}" "${INSTALL_DIR}/${BIN}"
fi

chmod +x "${INSTALL_DIR}/${BIN}"
echo "tunnel ${VERSION} installed to ${INSTALL_DIR}/${BIN}"

# Install man pages best-effort. ${HOME}/.local/share/man is on MANPATH for most
# distros; users who prefer system-wide can rerun with sudo and MAN_DIR=/usr/local/share/man.
MAN_DIR="${MAN_DIR:-${HOME}/.local/share/man/man1}"
if [ -d "${TMP}/man" ] && [ -n "$(ls -A "${TMP}/man" 2>/dev/null)" ]; then
  if mkdir -p "${MAN_DIR}" 2>/dev/null && [ -w "${MAN_DIR}" ]; then
    cp "${TMP}/man/"*.1 "${MAN_DIR}/" 2>/dev/null && \
      echo "man pages installed to ${MAN_DIR} (try: man tunnel)"
  else
    echo "Skipping man-page install (${MAN_DIR} not writable). Manually:"
    echo "  install -m 644 man/*.1 ~/.local/share/man/man1/"
  fi
fi

# Windows
# Install via Go (requires Go 1.23+):
#   go install github.com/anivaryam/tunnel/cmd/tunnel@latest
#   go install github.com/anivaryam/tunnel/cmd/server@latest
#
# Or download release binaries from:
#   https://github.com/anivaryam/tunnel/releases

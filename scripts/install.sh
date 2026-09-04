#!/bin/sh
# DCMS installer for macOS and Linux. Downloads the prebuilt binary for your
# platform from the latest GitHub release and installs it — no Go toolchain
# required.
#
#   curl -fsSL https://raw.githubusercontent.com/blazing-Gael/dcms/main/scripts/install.sh | sh
#
# Overrides (env vars):
#   DCMS_VERSION      a specific tag (e.g. v0.1.0-beta.1); default: latest
#   DCMS_INSTALL_DIR  where to put the binary; default: /usr/local/bin
#                     (falls back to ~/.local/bin if that isn't writable)
set -eu

REPO="blazing-Gael/dcms"
BIN="dcms"

info() { printf '%s\n' "$*" >&2; }
err() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

# ── Detect OS / arch and map to the release asset names ─────────────────────
os=$(uname -s)
case "$os" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) err "unsupported OS: $os (Windows users: use scripts/install.ps1)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) err "unsupported architecture: $arch" ;;
esac

asset="${BIN}_${os}_${arch}.tar.gz"

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar >/dev/null 2>&1 || err "tar is required"

# ── Resolve the release tag ─────────────────────────────────────────────────
# GitHub's /releases/latest is the latest *stable* release, so before a 1.0 it
# 404s while only pre-releases exist. Resolve the newest tag from the releases
# list (which includes pre-releases) instead.
tag="${DCMS_VERSION:-}"
if [ "$tag" = "" ] || [ "$tag" = "latest" ]; then
	tag=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases" \
		| grep -m1 '"tag_name":' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
	[ -n "$tag" ] || err "could not determine the latest release"
fi
url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
sums="https://github.com/${REPO}/releases/download/${tag}/checksums.txt"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

info "Downloading ${asset}..."
curl -fsSL "$url" -o "$tmp/$asset" || err "download failed: $url"

# ── Verify the checksum when possible (not fatal if the file is unavailable) ─
if curl -fsSL "$sums" -o "$tmp/checksums.txt" 2>/dev/null; then
	expected=$(grep " ${asset}\$" "$tmp/checksums.txt" 2>/dev/null | awk '{print $1}' || true)
	if [ -n "$expected" ]; then
		if command -v sha256sum >/dev/null 2>&1; then
			actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
		elif command -v shasum >/dev/null 2>&1; then
			actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
		else
			actual=""
		fi
		if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
			err "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
		fi
		[ -n "$actual" ] && info "Checksum verified."
	fi
fi

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/$BIN" ] || err "archive did not contain the ${BIN} binary"
chmod +x "$tmp/$BIN"

# ── Choose an install dir we can actually write to ──────────────────────────
dir="${DCMS_INSTALL_DIR:-/usr/local/bin}"
if [ ! -d "$dir" ] || [ ! -w "$dir" ]; then
	if [ -w "$dir" ] 2>/dev/null; then
		:
	elif command -v sudo >/dev/null 2>&1 && [ "${DCMS_INSTALL_DIR:-}" = "" ]; then
		info "Installing to $dir (needs sudo)..."
		sudo install -m 0755 "$tmp/$BIN" "$dir/$BIN" || err "install to $dir failed"
		printf 'done\n'
		info "Installed $("$dir/$BIN" version 2>/dev/null || echo "$BIN") to $dir/$BIN"
		exit 0
	else
		dir="$HOME/.local/bin"
		mkdir -p "$dir"
	fi
fi

install -m 0755 "$tmp/$BIN" "$dir/$BIN" 2>/dev/null || cp "$tmp/$BIN" "$dir/$BIN"
info "Installed $("$dir/$BIN" version 2>/dev/null || echo "$BIN") to $dir/$BIN"

case ":$PATH:" in
	*":$dir:"*) ;;
	*) info "Note: $dir is not on your PATH — add it, e.g.  export PATH=\"$dir:\$PATH\"" ;;
esac

info ""
info "Next:  dcms init myapp && cd myapp && dcms dev"

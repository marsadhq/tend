#!/bin/sh
# tend installer: one-line install for the tend job runner.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/marsadhq/tend/master/install.sh | sh
#
# Environment overrides:
#   PREFIX         Install prefix (default: /usr/local). Binary lands at $PREFIX/bin/tend.
#   VERSION        Install a specific version, e.g. 0.1.0 (skips the GitHub "latest" lookup).
#                  TEND_VERSION is accepted as an alias.
#   TEND_INSTALL_BASE_URL
#                  Override the download base URL. When set, assets are fetched directly
#                  from $TEND_INSTALL_BASE_URL/<asset> and $TEND_INSTALL_BASE_URL/checksums.txt;
#                  handy for serving a local goreleaser dist/ dir, e.g.:
#                    TEND_INSTALL_BASE_URL=http://localhost:8000 VERSION=0.0.1-snapshot-abc1234 sh install.sh
#                  This is intentionally distinct from TEND_BASE_URL (which tend uses at
#                  runtime for heartbeat ping URLs); the installer must not read that, or a
#                  configured runtime base URL would hijack where release assets come from.
#
# The script verifies the downloaded tarball against checksums.txt (sha256) and aborts
# if verification fails. It never installs an unverified binary.

set -eu

# ---- configuration -----------------------------------------------------------
REPO="marsadhq/tend"
PROJECT="tend"
PREFIX="${PREFIX:-/usr/local}"
# VERSION may come in as VERSION or TEND_VERSION.
VERSION="${VERSION:-${TEND_VERSION:-}}"
TEND_INSTALL_BASE_URL="${TEND_INSTALL_BASE_URL:-}"

# ---- output helpers ----------------------------------------------------------
info() {
	printf '%s\n' "$*"
}

err() {
	printf 'error: %s\n' "$*" >&2
}

fatal() {
	err "$*"
	exit 1
}

# ---- temp dir + cleanup ------------------------------------------------------
TMPDIR_INSTALL=""
cleanup() {
	if [ -n "$TMPDIR_INSTALL" ] && [ -d "$TMPDIR_INSTALL" ]; then
		rm -rf "$TMPDIR_INSTALL"
	fi
}
trap cleanup EXIT INT TERM

# ---- prerequisite tools ------------------------------------------------------
# Pick an HTTP downloader: prefer curl, fall back to wget.
DOWNLOADER=""
if command -v curl >/dev/null 2>&1; then
	DOWNLOADER="curl"
elif command -v wget >/dev/null 2>&1; then
	DOWNLOADER="wget"
else
	fatal "need curl or wget to download files; neither was found"
fi

# Pick a sha256 tool: prefer sha256sum, fall back to shasum -a 256.
SHATOOL=""
if command -v sha256sum >/dev/null 2>&1; then
	SHATOOL="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	SHATOOL="shasum"
else
	fatal "need sha256sum or shasum to verify downloads; neither was found"
fi

if ! command -v tar >/dev/null 2>&1; then
	fatal "need tar to extract the release archive; it was not found"
fi

# download <url> <dest>: fetch a URL to a file, failing on HTTP errors.
download() {
	# $1 = url, $2 = destination path
	if [ "$DOWNLOADER" = "curl" ]; then
		curl -fsSL "$1" -o "$2"
	else
		wget -qO "$2" "$1"
	fi
}

# fetch_stdout <url>: fetch a URL and print its body to stdout.
fetch_stdout() {
	if [ "$DOWNLOADER" = "curl" ]; then
		curl -fsSL "$1"
	else
		wget -qO - "$1"
	fi
}

# sha256_of <file>: print the lowercase hex sha256 of a file (hash only).
sha256_of() {
	if [ "$SHATOOL" = "sha256sum" ]; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# ---- OS / arch detection -----------------------------------------------------
os_raw="$(uname -s)"
case "$os_raw" in
	Linux) OS="linux" ;;
	*) fatal "unsupported OS '$os_raw'; tend release binaries are Linux-only" ;;
esac

arch_raw="$(uname -m)"
case "$arch_raw" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*) fatal "unsupported architecture '$arch_raw'; supported: x86_64/amd64, aarch64/arm64" ;;
esac

# ---- resolve version + base URL ----------------------------------------------
# TAG is the git tag (e.g. v0.1.0); VERSION is the assetless version (e.g. 0.1.0),
# which is the tag with any leading "v" stripped.
strip_v() {
	# print $1 with a single leading "v" removed, if present
	case "$1" in
		v*) printf '%s\n' "${1#v}" ;;
		*) printf '%s\n' "$1" ;;
	esac
}

TAG=""
if [ -n "$VERSION" ]; then
	# Caller pinned a version. Accept either "0.1.0" or "v0.1.0".
	VERSION="$(strip_v "$VERSION")"
	TAG="v${VERSION}"
	info "Using requested version ${VERSION}"
else
	info "Resolving latest tend release from GitHub..."
	api_url="https://api.github.com/repos/${REPO}/releases/latest"
	# Parse tag_name portably (no jq): grab the first "tag_name": "..." value.
	TAG="$(fetch_stdout "$api_url" \
		| grep -m1 '"tag_name"' \
		| sed -e 's/.*"tag_name"[[:space:]]*:[[:space:]]*"//' -e 's/".*//')"
	[ -n "$TAG" ] || fatal "could not resolve latest release tag from $api_url"
	VERSION="$(strip_v "$TAG")"
	info "Latest release is ${TAG} (version ${VERSION})"
fi

# Asset names follow goreleaser:
#   name_template: {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
ASSET="${PROJECT}_${VERSION}_${OS}_${ARCH}.tar.gz"
CHECKSUMS="checksums.txt"

# Base URL: explicit install-time override, else the GitHub release download path
# for the tag. The override is a dedicated TEND_INSTALL_BASE_URL (NOT TEND_BASE_URL,
# which tend uses at runtime), so a configured runtime base URL can never hijack the
# installer into fetching release assets from the wrong host.
if [ -n "$TEND_INSTALL_BASE_URL" ]; then
	# Trim a single trailing slash so we can join with "/" cleanly.
	BASE_URL="${TEND_INSTALL_BASE_URL%/}"
else
	BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
fi

ASSET_URL="${BASE_URL}/${ASSET}"
CHECKSUMS_URL="${BASE_URL}/${CHECKSUMS}"

# ---- download ----------------------------------------------------------------
TMPDIR_INSTALL="$(mktemp -d 2>/dev/null || mktemp -d -t tend-install)"
[ -n "$TMPDIR_INSTALL" ] && [ -d "$TMPDIR_INSTALL" ] || fatal "could not create a temp directory"

tarball_path="${TMPDIR_INSTALL}/${ASSET}"
checksums_path="${TMPDIR_INSTALL}/${CHECKSUMS}"

info "Downloading ${ASSET}..."
download "$ASSET_URL" "$tarball_path" || fatal "failed to download $ASSET_URL"

info "Downloading ${CHECKSUMS}..."
download "$CHECKSUMS_URL" "$checksums_path" || fatal "failed to download $CHECKSUMS_URL"

# ---- verify checksum (fail-closed) -------------------------------------------
info "Verifying checksum..."
# checksums.txt lines look like: "<sha256>  <filename>" (two spaces).
# Pull the expected hash for exactly our asset filename.
expected="$(awk -v f="$ASSET" '$2 == f {print $1; exit}' "$checksums_path")"
[ -n "$expected" ] || fatal "no checksum entry for ${ASSET} in ${CHECKSUMS}"

actual="$(sha256_of "$tarball_path")"
[ -n "$actual" ] || fatal "failed to compute sha256 of downloaded archive"

if [ "$expected" != "$actual" ]; then
	err "checksum mismatch for ${ASSET}"
	err "  expected: ${expected}"
	err "  actual:   ${actual}"
	fatal "refusing to install an unverified binary"
fi
info "Checksum OK (${actual})"

# ---- extract -----------------------------------------------------------------
info "Extracting..."
tar -xzf "$tarball_path" -C "$TMPDIR_INSTALL" || fatal "failed to extract $tarball_path"

bin_src="${TMPDIR_INSTALL}/${PROJECT}"
[ -f "$bin_src" ] || fatal "expected binary '${PROJECT}' not found in archive"
chmod +x "$bin_src"

# ---- install -----------------------------------------------------------------
dest_dir="${PREFIX}/bin"
dest="${dest_dir}/${PROJECT}"

# Decide whether we need sudo: we need it if we cannot create/write the target.
# Use sudo only when not already root and sudo exists.
SUDO=""
need_sudo() {
	# Returns success (0) if sudo is required to write to dest_dir.
	# Walk up to the nearest EXISTING ancestor of dest_dir and test whether it
	# is writable: `mkdir -p` will create the missing levels, so what matters is
	# the first directory that already exists (e.g. PREFIX=$HOME/.local where
	# .local does not exist yet but $HOME is writable → no sudo needed).
	d="$dest_dir"
	while [ ! -e "$d" ]; do
		parent="$(dirname "$d")"
		[ "$parent" = "$d" ] && break
		d="$parent"
	done
	[ ! -w "$d" ]
}

if need_sudo; then
	if [ "$(id -u)" -eq 0 ]; then
		SUDO=""
	elif command -v sudo >/dev/null 2>&1; then
		SUDO="sudo"
		info "Elevated permissions needed to write to ${dest_dir}; using sudo."
	else
		fatal "cannot write to ${dest_dir} and sudo is not available; re-run as root or set PREFIX to a writable location (e.g. PREFIX=\$HOME/.local)"
	fi
fi

info "Installing to ${dest}..."
$SUDO mkdir -p "$dest_dir" || fatal "failed to create ${dest_dir}"
$SUDO install -m 0755 "$bin_src" "$dest" 2>/dev/null \
	|| $SUDO cp "$bin_src" "$dest" \
	|| fatal "failed to install binary to ${dest}"
$SUDO chmod 0755 "$dest" || fatal "failed to set permissions on ${dest}"

# ---- done --------------------------------------------------------------------
info ""
info "tend ${VERSION} installed to ${dest}"
case ":${PATH}:" in
	*":${dest_dir}:"*) : ;;
	*) info "Note: ${dest_dir} is not on your PATH; add it or call ${dest} directly." ;;
esac
info ""
info "Next steps:"
info "  Run: tend version"
info "  Quickstart: https://github.com/${REPO}#60-second-quickstart"
info "  Then start the dashboard with 'tend serve' and open http://localhost:8080/login"

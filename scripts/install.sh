#!/bin/sh
# Installs the oak CLI from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/iagocavalcante/oakberryio/main/scripts/install.sh | sh
#
# Env overrides:
#   OAK_VERSION       release tag to install, e.g. "v1.2.3" (default: latest)
#   OAK_INSTALL_DIR   install directory (default: /usr/local/bin)
#
# POSIX sh only (no bashisms): this runs via `curl | sh` on whatever /bin/sh
# a stranger's machine has, so it must not assume bash. Deliberately avoids
# a blanket `set -e`: every command that can fail is checked explicitly and
# reported with a clear message, and a trap always cleans up the temp dir.

REPO="iagocavalcante/oakberryio"
INSTALL_DIR="${OAK_INSTALL_DIR:-/usr/local/bin}"

TMPDIR=""

cleanup() {
	[ -n "$TMPDIR" ] && rm -rf "$TMPDIR"
}
trap cleanup EXIT INT TERM HUP

fail() {
	echo "install.sh: error: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "required tool '$1' not found in PATH"
}

need curl
need tar

# Pick a sha256 tool: Linux typically has sha256sum, macOS has shasum.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		fail "no sha256 tool found (need sha256sum or shasum)"
	fi
}

# --- detect platform -------------------------------------------------------

os_raw=$(uname -s)
case "$os_raw" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported OS: $os_raw (oak only ships darwin and linux builds)" ;;
esac

arch_raw=$(uname -m)
case "$arch_raw" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported architecture: $arch_raw (oak only ships amd64 and arm64 builds)" ;;
esac

# --- resolve version --------------------------------------------------------

if [ -n "${OAK_VERSION:-}" ]; then
	version="$OAK_VERSION"
else
	echo "Resolving latest release..." >&2
	api_response=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest") \
		|| fail "could not reach GitHub API to resolve the latest release (set OAK_VERSION to skip this)"
	version=$(echo "$api_response" | grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
	[ -n "$version" ] || fail "could not parse a release tag from the GitHub API response"
fi

echo "Installing oak ${version} (${os}/${arch})..." >&2

# --- download ---------------------------------------------------------------

TMPDIR=$(mktemp -d) || fail "could not create a temp directory"

archive="oakberryio_${os}_${arch}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${version}"

curl -fsSL -o "${TMPDIR}/${archive}" "${base_url}/${archive}" \
	|| fail "could not download ${archive} for release ${version} (does that release/platform combination exist?)"

curl -fsSL -o "${TMPDIR}/checksums.txt" "${base_url}/checksums.txt" \
	|| fail "could not download checksums.txt for release ${version}"

# --- verify checksum ---------------------------------------------------------

expected=$(grep " ${archive}\$" "${TMPDIR}/checksums.txt" | awk '{print $1}')
[ -n "$expected" ] || fail "no checksum entry for ${archive} in checksums.txt"

actual=$(sha256 "${TMPDIR}/${archive}")
[ "$expected" = "$actual" ] || fail "checksum mismatch for ${archive}: expected ${expected}, got ${actual}"

# --- extract and install -----------------------------------------------------

tar -xzf "${TMPDIR}/${archive}" -C "$TMPDIR" oak \
	|| fail "could not extract 'oak' from ${archive}"

[ -f "${TMPDIR}/oak" ] || fail "extracted archive did not contain an 'oak' binary"
chmod +x "${TMPDIR}/oak"

if [ -w "$INSTALL_DIR" ] || [ -w "$(dirname "$INSTALL_DIR")" ]; then
	mkdir -p "$INSTALL_DIR" || fail "could not create ${INSTALL_DIR}"
	mv "${TMPDIR}/oak" "${INSTALL_DIR}/oak" || fail "could not install to ${INSTALL_DIR}/oak"
else
	need sudo
	echo "Installing to ${INSTALL_DIR} requires elevated privileges." >&2
	sudo mkdir -p "$INSTALL_DIR" || fail "could not create ${INSTALL_DIR}"
	sudo mv "${TMPDIR}/oak" "${INSTALL_DIR}/oak" || fail "could not install to ${INSTALL_DIR}/oak"
fi

installed="${INSTALL_DIR}/oak"
echo "Installed ${installed}:" >&2
"$installed" --version || fail "installed binary at ${installed} failed to run"

case ":$PATH:" in
	*":${INSTALL_DIR}:"*) ;;
	*) echo "Note: ${INSTALL_DIR} is not on your PATH." >&2 ;;
esac

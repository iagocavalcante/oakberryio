#!/bin/sh
# Turns a fresh Ubuntu Server 24.04 machine into an oakberryio box.
#
#   curl -fsSL https://oakberryio.iagocavalcante.com/box.sh | sudo OAK_DOMAIN=apps.example.com bash
#
# It downloads the versioned oak release (binaries + host-setup + units,
# checksum-verified), installs them, generates a UNIQUE api token for THIS
# box, runs the host bootstrap, and starts oakd. The only step left after is
# connecting the Cloudflare Tunnel (it prints how).
#
# Env overrides:
#   OAK_DOMAIN     apps domain, e.g. apps.example.com   (required)
#   OAK_VERSION    release tag to install, default: latest
#   OAK_REPO       GitHub repo, default iagocavalcante/oakberryio
set -eu

REPO="${OAK_REPO:-iagocavalcante/oakberryio}"
API_BASE="https://api.github.com/repos/${REPO}"
DL_BASE="https://github.com/${REPO}/releases"

fail() { echo "box.sh: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---- preflight -------------------------------------------------------------
[ "$(id -u)" = "0" ] || fail "run as root (curl ... | sudo bash)"
have curl || fail "curl is required"
have tar || fail "tar is required"

uname_s="$(uname -s)"
[ "$uname_s" = "Linux" ] || fail "oak boxes are Linux only (got $uname_s)"

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) goarch="amd64" ;;
  aarch64|arm64) fail "arm64 is not supported yet: the guest kernel oak downloads is x86_64-only. Use an x86_64 box." ;;
  *) fail "unsupported architecture: $arch" ;;
esac

if ! grep -qE 'svm|vmx' /proc/cpuinfo 2>/dev/null; then
  fail "no CPU virtualization (svm/vmx) — enable SVM (AMD) or VT-x (Intel) in BIOS"
fi
[ -e /dev/kvm ] || fail "/dev/kvm missing — enable virtualization in BIOS and reboot"

# ---- OAK_DOMAIN ------------------------------------------------------------
# curl|sudo bash pipes the SCRIPT to stdin, so prompt via /dev/tty when the
# domain wasn't passed as an env var.
if [ -z "${OAK_DOMAIN:-}" ]; then
  if [ -r /dev/tty ]; then
    printf 'Apps domain (e.g. apps.example.com): ' > /dev/tty
    read -r OAK_DOMAIN < /dev/tty || true
  fi
fi
[ -n "${OAK_DOMAIN:-}" ] || fail "OAK_DOMAIN is required — re-run with: sudo OAK_DOMAIN=apps.example.com bash"

# ---- checksum helper -------------------------------------------------------
sha256() {
  if have sha256sum; then sha256sum "$1" | awk '{print $1}';
  elif have shasum; then shasum -a 256 "$1" | awk '{print $1}';
  else fail "need sha256sum or shasum to verify the download"; fi
}

# ---- resolve version -------------------------------------------------------
version="${OAK_VERSION:-}"
if [ -z "$version" ]; then
  echo "box.sh: resolving latest release..."
  version="$(curl -fsSL "${API_BASE}/releases/latest" \
    | grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -n "$version" ] || fail "could not resolve latest release of ${REPO} (is it published with a release yet?)"
fi
echo "box.sh: installing oak ${version} (linux/${goarch}) for domain ${OAK_DOMAIN}"

# ---- download + verify -----------------------------------------------------
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM HUP

archive="oakberryio_linux_${goarch}.tar.gz"
curl -fSL --retry 3 -o "${tmp}/${archive}" "${DL_BASE}/download/${version}/${archive}" \
  || fail "download failed: ${archive} for ${version}"
curl -fsSL -o "${tmp}/checksums.txt" "${DL_BASE}/download/${version}/checksums.txt" \
  || fail "download failed: checksums.txt"

want="$(awk -v f="$archive" '$2 == f {print $1}' "${tmp}/checksums.txt")"
[ -n "$want" ] || fail "${archive} not listed in checksums.txt"
got="$(sha256 "${tmp}/${archive}")"
[ "$want" = "$got" ] || fail "checksum mismatch for ${archive} (expected ${want}, got ${got})"

tar -xzf "${tmp}/${archive}" -C "$tmp" || fail "could not extract ${archive}"

# ---- install binaries + units ---------------------------------------------
echo "box.sh: installing binaries and units"
install -m755 "${tmp}/oakd"     /usr/local/bin/oakd
install -m755 "${tmp}/oak-init" /usr/local/bin/oak-init
install -m755 "${tmp}/oak"      /usr/local/bin/oak
mkdir -p /opt/oak /etc/oak
install -m755 "${tmp}/scripts/host-setup.sh" /opt/oak/host-setup.sh
install -m755 "${tmp}/scripts/backup.sh"     /usr/local/bin/oak-backup.sh
install -m644 "${tmp}/deploy/oakd.service"                /etc/systemd/system/oakd.service
install -m644 "${tmp}/deploy/oak-backup.service"          /etc/systemd/system/oak-backup.service
install -m644 "${tmp}/deploy/oak-backup.timer"            /etc/systemd/system/oak-backup.timer
install -m644 "${tmp}/deploy/oak-docker-forward.service"  /etc/systemd/system/oak-docker-forward.service

# ---- oakd.toml with a UNIQUE per-box token --------------------------------
if [ -f /etc/oak/oakd.toml ]; then
  echo "box.sh: keeping existing /etc/oak/oakd.toml"
else
  have openssl || fail "openssl is required to generate an api token"
  token="$(openssl rand -hex 32)"
  umask 077
  cat >/etc/oak/oakd.toml <<EOF
data_dir = "/var/lib/oak"
registry = "localhost:5000"
domain = "${OAK_DOMAIN}"
tunnel_id = ""
tunnel_config = "/etc/cloudflared/config.yml"
key_file = "/etc/oak/key"
socket = "/run/oak/oak.sock"
log_dir = "/var/log/oak"
api_token = "${token}"
api_port = 7000
EOF
  chmod 600 /etc/oak/oakd.toml
  echo "box.sh: generated /etc/oak/oakd.toml with a fresh api_token"
fi

# ---- host bootstrap --------------------------------------------------------
echo "box.sh: running host bootstrap (Firecracker, kernel, nftables, registry, cloudflared)..."
OAK_DOMAIN="$OAK_DOMAIN" /opt/oak/host-setup.sh

# ---- services --------------------------------------------------------------
systemctl daemon-reload
systemctl enable --now oak-docker-forward.service >/dev/null 2>&1 || true
systemctl enable --now oak-backup.timer >/dev/null 2>&1 || true
systemctl enable --now oakd

cat <<EOF

box.sh: oakd is installed and running. One step left — the Cloudflare Tunnel:

  1. cloudflared tunnel login
  2. cloudflared tunnel create oak      # note the tunnel id it prints
  3. put that id in /etc/oak/oakd.toml  (tunnel_id = "...")
  4. add a wildcard DNS record: *.${OAK_DOMAIN} CNAME <tunnel-id>.cfargotunnel.com
  5. sudo systemctl restart oakd

Then deploy an app from your laptop with:  oak deploy
Docs: https://github.com/${REPO}#readme
EOF

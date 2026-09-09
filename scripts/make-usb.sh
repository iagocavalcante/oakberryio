#!/usr/bin/env bash
# Builds a bootable Ubuntu Server 24.04 autoinstall USB stick for the oak
# box, on the Mac. Requires `brew install xorriso coreutils gettext` (the
# last for envsubst).
#
# Required env:
#   OAK_HOSTNAME       box hostname, e.g. oak
#   OAK_SSH_KEY        one public key line, e.g. "$(cat ~/.ssh/id_ed25519.pub)"
#   OAK_DOMAIN         apps domain, e.g. apps.example.com
#   OAK_PASSWORD_HASH  `openssl passwd -6` output for the `oak` user
# Optional env:
#   DEV                macOS disk to write the finished ISO to, e.g.
#                      /dev/disk4 -- NEVER touched unless this is set; see
#                      docs/usb.md for how to find the right one
#   OAK_UBUNTU_VERSION Ubuntu point release, default 24.04.4
set -euo pipefail

: "${OAK_HOSTNAME:?set OAK_HOSTNAME}"
: "${OAK_SSH_KEY:?set OAK_SSH_KEY}"
: "${OAK_DOMAIN:?set OAK_DOMAIN}"
: "${OAK_PASSWORD_HASH:?set OAK_PASSWORD_HASH}"

UBUNTU_VERSION="${OAK_UBUNTU_VERSION:-24.04.4}"
UBUNTU_SERIES="${UBUNTU_VERSION%.*}" # 24.04.4 -> 24.04
ISO_NAME="ubuntu-${UBUNTU_VERSION}-live-server-amd64.iso"
ISO_URL="https://releases.ubuntu.com/${UBUNTU_SERIES}/${ISO_NAME}"
SUMS_URL="https://releases.ubuntu.com/${UBUNTU_SERIES}/SHA256SUMS"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD="${ROOT}/build"
mkdir -p "$BUILD"

echo "make-usb: 1/6 fetching and verifying ${ISO_NAME}"
iso_path="${BUILD}/${ISO_NAME}"
if [ ! -f "$iso_path" ]; then
	curl -fsSL -o "$iso_path" "$ISO_URL"
fi
sums_path="${BUILD}/SHA256SUMS"
curl -fsSL -o "$sums_path" "$SUMS_URL"
expected="$(awk -v f="$ISO_NAME" '$2 == "*"f || $2 == f {print $1}' "$sums_path")"
if [ -z "$expected" ]; then
	echo "make-usb: ${ISO_NAME} not listed in ${SUMS_URL}" >&2
	exit 1
fi
actual="$(shasum -a 256 "$iso_path" | awk '{print $1}')"
if [ "$expected" != "$actual" ]; then
	echo "make-usb: SHA256 mismatch for ${ISO_NAME}" >&2
	echo "  expected ${expected}" >&2
	echo "  got      ${actual}" >&2
	exit 1
fi

echo "make-usb: 2/6 building oakd/oak-init"
( cd "$ROOT" && make build )

echo "make-usb: 3/6 staging /opt/oak payload"
stage="${BUILD}/oak"
rm -rf "$stage"
mkdir -p "${stage}/bin"
cp "${ROOT}/scripts/host-setup.sh" "$stage/"
cp "${ROOT}/scripts/backup.sh" "$stage/"
cp "${ROOT}/bin/oakd" "${stage}/bin/"
cp "${ROOT}/bin/oak-init" "${stage}/bin/"
cp "${ROOT}/deploy/oakd.service" "$stage/"
cp "${ROOT}/deploy/oak-backup.service" "$stage/"
cp "${ROOT}/deploy/oak-backup.timer" "$stage/"
cp "${ROOT}/deploy/oak-firstboot.service" "$stage/"
cp "${ROOT}/deploy/oak-docker-forward.service" "$stage/"

api_token="$(openssl rand -hex 32)"
cat >"${stage}/oakd.toml" <<EOF
data_dir = "/var/lib/oak"
registry = "localhost:5000"
domain = "${OAK_DOMAIN}"
tunnel_id = ""
tunnel_config = "/etc/cloudflared/config.yml"
key_file = "/etc/oak/key"
socket = "/run/oak.sock"
log_dir = "/var/log/oak"
api_token = "${api_token}"
api_port = 7000
EOF

echo "make-usb: 4/6 rendering autoinstall/user-data"
autoinstall="${BUILD}/autoinstall"
rm -rf "$autoinstall"
mkdir -p "$autoinstall"
export OAK_HOSTNAME OAK_SSH_KEY OAK_DOMAIN OAK_PASSWORD_HASH
envsubst '${OAK_HOSTNAME} ${OAK_SSH_KEY} ${OAK_DOMAIN} ${OAK_PASSWORD_HASH}' \
	<"${ROOT}/usb/user-data.tmpl" >"${autoinstall}/user-data"
cp "${ROOT}/usb/meta-data" "${autoinstall}/meta-data"

echo "make-usb: 5/6 repacking ${ISO_NAME}"
# Pull out grub.cfg to patch its kernel line locally, then rebuild the ISO
# from the *original* as -indev with the new autoinstall/ and oak/ trees
# mapped in and grub.cfg swapped, keeping the source's existing El Torito +
# hybrid MBR/GPT boot catalog via `-boot_image any replay` rather than
# re-authoring one from scratch.
work="${BUILD}/iso-work"
rm -rf "$work"
mkdir -p "$work"
xorriso -indev "$iso_path" -osirrox on -extract /boot/grub/grub.cfg "${work}/grub.cfg"
chmod u+w "${work}/grub.cfg"
# ponytail: this regex targets the standard Ubuntu 24.04 live-server
# menuentry ("linux /casper/vmlinuz ..."); if a future point release
# reformats grub.cfg, verify the patched file (build/iso-work/grub.cfg)
# actually gained the autoinstall parameter before writing to a USB stick.
sed -i '' -E 's#(linux[[:space:]]+/casper/vmlinuz)#\1 autoinstall ds=nocloud\\;s=/cdrom/autoinstall/#' \
	"${work}/grub.cfg"

out_iso="${BUILD}/oak-install.iso"
rm -f "$out_iso"
xorriso -indev "$iso_path" -outdev "$out_iso" \
	-boot_image any replay \
	-map "$autoinstall" /autoinstall \
	-map "$stage" /oak \
	-update "${work}/grub.cfg" /boot/grub/grub.cfg \
	-commit

echo "make-usb: 6/6 done"
echo "make-usb: built ${out_iso}"
echo "make-usb: oakd api_token = ${api_token}"
echo "make-usb: (also saved in ${stage}/oakd.toml, which is now baked into the ISO)"

if [ -n "${DEV:-}" ]; then
	rdev="${DEV/disk/rdisk}"
	diskutil unmountDisk "$DEV"
	sudo dd if="$out_iso" of="$rdev" bs=4m status=progress
	diskutil eject "$DEV" || true
	echo "make-usb: wrote ${out_iso} to ${DEV}"
else
	echo "make-usb: DEV not set -- leaving ${out_iso} on disk; see docs/usb.md to write it"
fi

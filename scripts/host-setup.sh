#!/usr/bin/env bash
# Run as root on Ubuntu Server 24.04. Idempotent.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.10.1}"
OAK_DOMAIN="${OAK_DOMAIN:?set OAK_DOMAIN=apps.example.com}"

grep -q -E 'svm|vmx' /proc/cpuinfo || { echo "no virtualization flag: enable SVM in BIOS"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing"; exit 1; }

apt-get update
apt-get install -y curl jq nftables e2fsprogs docker.io age

# firecracker
if ! command -v firecracker >/dev/null; then
  arch=$(uname -m)
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${arch}.tgz" | tar xz -C /tmp
  install -m755 "/tmp/release-${FC_VERSION}-${arch}/firecracker-${FC_VERSION}-${arch}" /usr/local/bin/firecracker
fi

# guest kernel (Firecracker CI kernel, good enough for v1)
mkdir -p /var/lib/oak/{images,rootfs,volumes,kernel} /var/log/oak /etc/oak
[ -f /var/lib/oak/kernel/vmlinux ] || \
  curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-6.1.102" -o /var/lib/oak/kernel/vmlinux

# secrets key
[ -f /etc/oak/key ] || { age-keygen -o /etc/oak/key; chmod 600 /etc/oak/key; }

# bridge
cat >/etc/systemd/network/oak0.netdev <<'N'
[NetDev]
Name=oak0
Kind=bridge
N
cat >/etc/systemd/network/oak0.network <<'N'
[Match]
Name=oak0
[Network]
Address=10.200.0.1/16
IPForward=yes
N
systemctl enable --now systemd-networkd
networkctl reload

# nat
cat >/etc/nftables.conf <<'N'
flush ruleset
table ip nat {
  chain postrouting { type nat hook postrouting priority 100; oifname != "oak0" ip saddr 10.200.0.0/16 masquerade; }
}
N
systemctl enable --now nftables
nft -f /etc/nftables.conf
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/99-oak.conf

# local registry
docker ps -q -f name=oak-registry | grep -q . || \
  docker run -d --restart=always --name oak-registry -p 127.0.0.1:5000:5000 -v /var/lib/oak/registry:/var/lib/registry registry:2

# cloudflared
if ! command -v cloudflared >/dev/null; then
  curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
  chmod +x /usr/local/bin/cloudflared
fi
echo "DONE. Next: cloudflared tunnel login && cloudflared tunnel create oak, then put tunnel id in /etc/oak/oakd.toml"

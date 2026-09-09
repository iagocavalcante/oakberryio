#!/usr/bin/env bash
# Run as root on Ubuntu Server 24.04. Idempotent.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.10.1}"
OAK_DOMAIN="${OAK_DOMAIN:?set OAK_DOMAIN=apps.example.com}"

grep -q -E 'svm|vmx' /proc/cpuinfo || { echo "no virtualization flag: enable SVM in BIOS"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing"; exit 1; }

apt-get update
apt-get install -y curl jq nftables e2fsprogs docker.io age sqlite3 rsync

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
ConfigureWithoutCarrier=yes
[Link]
RequiredForOnline=no
N
systemctl enable --now systemd-networkd
networkctl reload

# nftables: Docker manages its own nftables/iptables state (including a
# FORWARD DROP policy), so this must not `flush ruleset` -- that would wipe
# Docker's rules on every idempotent re-run -- and oak0's forwarded traffic
# needs its own explicit accept since Docker's DROP policy would otherwise
# catch it too. oak's rules live in their own table/file, included from
# /etc/nftables.conf rather than replacing it.
mkdir -p /etc/nftables.d
cat >/etc/nftables.d/oak.nft <<'N'
table ip oak {
  chain forward {
    type filter hook forward priority -10;
    iifname "oak0" accept
    oifname "oak0" ct state related,established accept
    oifname "oak0" accept
  }
  chain postrouting {
    type nat hook postrouting priority 100;
    oifname != "oak0" ip saddr 10.200.0.0/16 masquerade
  }
}
N
if [ ! -f /etc/nftables.conf ]; then
  printf '#!/usr/sbin/nft -f\ninclude "/etc/nftables.d/oak.nft"\n' >/etc/nftables.conf
elif ! grep -q 'include "/etc/nftables.d/oak.nft"' /etc/nftables.conf; then
  echo 'include "/etc/nftables.d/oak.nft"' >>/etc/nftables.conf
fi
nft delete table ip oak 2>/dev/null || true
nft -f /etc/nftables.d/oak.nft
systemctl enable --now nftables
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/99-oak.conf

# Docker's own FORWARD chain policy is DROP, and in nftables an `accept`
# verdict only ends traversal of the chain it's in -- oak's forward chain
# above does not stop the packet from also hitting docker's DROP policy in
# `ip filter FORWARD`. Docker's supported extension point for exactly this
# is its DOCKER-USER chain, which it always jumps to first from FORWARD and
# never overwrites once populated. Docker does, however, recreate
# DOCKER-USER empty every time it starts, so the rule has to be reapplied on
# every docker.service start, not just once here.
cat >/usr/local/bin/oak-docker-user-rules.sh <<'N'
#!/usr/bin/env bash
set -euo pipefail
iptables -C DOCKER-USER -i oak0 -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -i oak0 -j ACCEPT
iptables -C DOCKER-USER -o oak0 -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -o oak0 -j ACCEPT
N
chmod 755 /usr/local/bin/oak-docker-user-rules.sh
mkdir -p /etc/systemd/system/docker.service.d
cat >/etc/systemd/system/docker.service.d/10-oak-user-chain.conf <<'N'
[Service]
ExecStartPost=/usr/local/bin/oak-docker-user-rules.sh
N
systemctl daemon-reload
# Apply immediately too, in case docker is already running and won't be
# restarted by this script (the drop-in only fires on docker's own future
# starts).
/usr/local/bin/oak-docker-user-rules.sh

# local registry
docker ps -q -f name=oak-registry | grep -q . || \
  docker run -d --restart=always --name oak-registry -p 127.0.0.1:5000:5000 -v /var/lib/oak/registry:/var/lib/registry registry:2

# cloudflared
if ! command -v cloudflared >/dev/null; then
  curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
  chmod +x /usr/local/bin/cloudflared
fi
echo "DONE. Next: cloudflared tunnel login && cloudflared tunnel create oak, then put tunnel id in /etc/oak/oakd.toml"

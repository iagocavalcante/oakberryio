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

# `networkctl reload` re-reads every .network file (including netplan's for the
# uplink), which tears down and rebuilds the uplink's DNS for a few seconds --
# during that window name resolution fails, and the download steps below would
# die with "Could not resolve host". Wait for resolution to recover before
# proceeding rather than racing it.
for _ in $(seq 1 30); do
  getent hosts github.com >/dev/null 2>&1 && break
  sleep 1
done

# nftables: Docker manages its own nftables/iptables state (including a
# FORWARD DROP policy), so this must not `flush ruleset` -- that would wipe
# Docker's rules on every idempotent re-run -- and oak*'s forwarded traffic
# needs its own explicit accept since Docker's DROP policy would otherwise
# catch it too. oak's rules live in their own table/file, included from
# /etc/nftables.conf rather than replacing it.
#
# The ruleset is static and generic across every tenant bridge (oak0, oak1,
# ...  oakN): only bridges are created dynamically (oakd's ensureBridge, see
# docs/plans/2026-09-15-phase-d-network-isolation-design.md), so nftables
# never needs rewriting per tenant.
#   - forward: same-bridge (intra-tenant) traffic is switched at L2 and
#     never reaches this chain, so "oak* -> oak*" here only ever matches
#     *cross*-bridge traffic -- i.e. cross-tenant -- which is dropped. This
#     also drops admin<->tenant over the network, which is intentional: the
#     admin manages tenants via the control plane, not by dialing their VMs.
#   - postrouting: masquerade any tenant subnet's traffic that isn't itself
#     staying within an oak* bridge, so tenant->internet egress still works.
#   - input: each bridge's gateway runs its own DNS listener (a tenant's
#     nameserver is its own gateway; cross-subnet DNS would be dropped by
#     the forward rule above like any other cross-tenant traffic), so
#     tenants need to reach port 53 on their own gateway.
mkdir -p /etc/nftables.d
cat >/etc/nftables.d/oak.nft <<'N'
table ip oak {
  chain forward {
    type filter hook forward priority -10;
    oifname "oak*" ct state related,established accept
    iifname "oak*" oifname "oak*" drop
    iifname "oak*" accept
  }
  chain postrouting {
    type nat hook postrouting priority 100;
    oifname != "oak*" ip saddr 10.200.0.0/16 masquerade
  }
  chain input {
    type filter hook input priority -10;
    iifname "oak*" udp dport 53 accept
    iifname "oak*" tcp dport 53 accept
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
# never overwrites once populated. deploy/oak-docker-forward.service (kept
# in sync with the unit written here -- this script has to be able to set
# the rule up on its own when copied and run standalone per docs/host.md,
# without deploy/ necessarily alongside it) applies it via idempotent
# `iptables -C ... || iptables -I ...` checks, After/Requires=docker.service
# so it runs whenever docker does.
#
# The rules mirror the oak table's forward chain above (oak*'s cross-tenant
# drop, then its accept) using iptables' "+" interface wildcard for the
# same oak0/oak1/.../oakN generality. In practice the cross-tenant drop
# here is unreachable -- the "ip oak" table above hooks at a lower priority
# (-10) than Docker's own filter table (0), so a cross-tenant packet is
# already dropped before it ever reaches DOCKER-USER -- but it's kept as
# defense-in-depth so this chain's own policy doesn't depend on that
# ordering. ExecStart order matters: iptables -I inserts at the top of the
# chain each time, so these run in reverse of the desired final order
# (drop on top, then the two accepts).
cat >/etc/systemd/system/oak-docker-forward.service <<'N'
[Unit]
Description=allow oak* traffic through Docker's DOCKER-USER chain
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'iptables -C DOCKER-USER -o oak+ -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -o oak+ -j ACCEPT'
ExecStart=/bin/sh -c 'iptables -C DOCKER-USER -i oak+ -j ACCEPT 2>/dev/null || iptables -I DOCKER-USER -i oak+ -j ACCEPT'
ExecStart=/bin/sh -c 'iptables -C DOCKER-USER -i oak+ -o oak+ -j DROP 2>/dev/null || iptables -I DOCKER-USER -i oak+ -o oak+ -j DROP'

[Install]
WantedBy=multi-user.target
N
systemctl daemon-reload
systemctl enable --now oak-docker-forward.service

# local registry -- idempotent across all container states: `docker ps` lists
# only *running* containers, so a stopped-but-existing oak-registry would fall
# through to `docker run --name oak-registry` and fail with a name conflict.
# Match on the exact name against *all* containers and start-or-run accordingly.
if [ -n "$(docker ps -q -f name='^oak-registry$')" ]; then
  :  # already running
elif [ -n "$(docker ps -aq -f name='^oak-registry$')" ]; then
  docker start oak-registry
else
  docker run -d --restart=always --name oak-registry -p 127.0.0.1:5000:5000 -v /var/lib/oak/registry:/var/lib/registry registry:2
fi

# cloudflared
if ! command -v cloudflared >/dev/null; then
  curl -fsSL --retry 5 --retry-delay 2 --retry-all-errors https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
  chmod +x /usr/local/bin/cloudflared
fi
echo "DONE. Next: cloudflared tunnel login && cloudflared tunnel create oak, then put tunnel id in /etc/oak/oakd.toml"

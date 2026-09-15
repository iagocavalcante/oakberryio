# Phase D — data-plane network isolation (per-tenant subnets)

Ownership is control-plane only today: every VM sits on one `oak0` bridge in
`10.200.0.0/16`, so a tenant's VM can reach any other tenant's VM (L2, so it
never even hits nftables) and can resolve + connect to `*.internal`, including
the admin's databases. This makes tenant traffic actually isolated so an
allowlisted-but-untrusted friend can deploy without reaching your data.

**Chosen model: per-tenant subnets/bridges.** Each owner gets a `/24` and its
own Linux bridge. Cross-tenant traffic then has to be *routed* through the host,
where nftables `FORWARD` drops it; intra-tenant stays L2 within the bridge;
internet egress is masqueraded as before. Rejected the single-bridge +
`br_netfilter` + IP-set alternative: `br_netfilter` is a host-global toggle that
would also subject Docker's bridges (registry, oak-panel) to nftables filtering,
which historically breaks Docker networking. Per-tenant bridges keep the blast
radius inside oak.

## Address plan

Split `10.200.0.0/16` into `/24`s indexed by tenant:

| idx | owner            | subnet          | gateway      | bridge |
|-----|------------------|-----------------|--------------|--------|
| 0   | `""` (admin/legacy) | 10.200.0.0/24 | 10.200.0.1   | `oak0` |
| 1   | first tenant     | 10.200.1.0/24   | 10.200.1.1   | `oak1` |
| n   | nth tenant       | 10.200.n.0/24   | 10.200.n.1   | `oakn` |

- **idx 0 is the default**, owner `""`. This is backwards-compatible: existing
  VMs are already `10.200.0.x` with gateway `10.200.0.1`, and `oak0` already
  exists. The admin (Iago) and any owner-less legacy app live here.
- Bridge names are `oak<idx>` (short, within the 15-char ifname limit up to
  `oak255`, and a clean `oak*` nftables wildcard). One `/24` = 253 usable VM
  IPs (`.2`–`.254`, `.1` = gateway); up to 256 tenants. Ample for a homelab.
- `oakd` owns bridge lifecycle from idx 1 up (created on demand). `host-setup.sh`
  keeps creating `oak0` (idx 0) so the box has working networking before oakd
  starts.

## oakd changes (Go)

### store: owner → subnet index, subnet-scoped IPAM
- New table `tenant_subnets(owner TEXT PRIMARY KEY, idx INTEGER NOT NULL UNIQUE)`,
  seeded with `('', 0)` on migrate.
- `SubnetForOwner(owner) (idx int, err error)` — get-or-allocate the smallest
  free `idx >= 1` for a new owner, in a `BEGIN IMMEDIATE` tx (same serialization
  as `AllocAndInsertMachine`).
- `allocIP` becomes subnet-scoped: scan `machines.ip` within `10.200.<idx>.0/24`
  and return the smallest free `.2`–`.254`. `AllocAndInsertMachine` gains an
  `idx` param; deploy passes the app owner's idx.

### daemon: per-tenant bridge + gateway + DNS listener
- `ensureBridge(idx)` — idempotent: create `oak<idx>` via netlink if absent,
  assign `10.200.<idx>.1/24`, bring it up. Tolerates `oak0` already existing
  (created by host-setup). Called before booting a VM.
- Each bridge's gateway needs a DNS listener, because a tenant's nameserver is
  its own gateway (`10.200.<idx>.1`) and cross-subnet DNS to `10.200.0.1` would
  be dropped by the cross-tenant rule. `ensureBridge` starts a
  `dns.Server` on `10.200.<idx>.1:53` (idx 0's listener replaces the current
  static one). All listeners share one resolver.
- Deploy/boot: look up `apps.owner` → `SubnetForOwner` → idx; `AllocAndInsertMachine(..., idx)`;
  `ensureBridge(idx)`; boot the VM with the right bridge + gateway.

### vm: bridge + mask from spec
- `Spec` gains `Bridge string`. `createTap` attaches to `spec.Bridge` instead of
  the hardcoded `"oak0"`.
- Machine IP is now `ip + "/24"` (was `/16`) and `Gateway`/`DNS` are the
  per-subnet `10.200.<idx>.1`. This is what makes a VM ARP its gateway for
  off-subnet destinations so the host can route + filter — the crux of the
  isolation.

### dns: scope answers by requester
- `Resolve` signature changes to take the requester's source IP (from
  `w.RemoteAddr()`): `Resolve func(app string, srcIP net.IP) []net.IP`.
- The resolver maps `srcIP` → idx (`/24`) → owner via the store, and returns A
  records only for machines whose app is owned by that owner. A source outside
  every tenant subnet (the host itself) is treated as admin and resolves all.
- An app that exists but isn't yours resolves as **NXDOMAIN** (don't leak
  existence). Existing NODATA-for-non-A behavior is preserved for apps you *can*
  see.

## Host / nftables changes (`scripts/host-setup.sh`)

The ruleset is **static and generic** (owner-agnostic) — only bridges are
dynamic, so nftables never needs rewriting per tenant. Replace the oak forward
chain body with, in order:

```
chain forward {
  type filter hook forward priority -10;
  oifname "oak*" ct state related,established accept   # return traffic
  iifname "oak*" oifname "oak*" drop                   # cross-tenant (same-bridge is L2, never routed here)
  iifname "oak*" accept                                # tenant -> internet
}
chain postrouting {
  type nat hook postrouting priority 100;
  oifname != "oak*" ip saddr 10.200.0.0/16 masquerade
}
```

- Same-bridge (intra-tenant) traffic is L2-switched and never reaches `FORWARD`,
  so the `oak*→oak*` rule matches only *cross*-bridge = cross-tenant → drop.
  This also drops admin↔tenant over the network (fine; admin manages via the
  control plane, not by dialing tenant VMs).
- The same DOCKER-USER jump that exists today still carries oak's forwarded
  traffic past Docker's `FORWARD` DROP policy; the DOCKER-USER body mirrors the
  forward chain's accept/drop for `oak*`.
- INPUT: allow `udp/tcp dport 53 iifname "oak*"` so tenants can reach their
  gateway's DNS; oakd's API stays `127.0.0.1`-only and is never on a bridge IP.

## Rollout (live box — needs a short window)

Existing VMs (`iago`, `misesnag`) are on the flat `/16` and won't be isolated
until re-homed onto a `/24`. They're admin-owned → idx 0 → still `oak0` /
`10.200.0.x`, but with the corrected `/24` mask they must be **restarted** to
pick it up. Steps:
1. Ship + deploy new oakd; run updated `host-setup.sh` (idempotent) to install
   the new nftables ruleset.
2. `oak restart iago && oak restart misesnag` so both re-home with `/24`.
3. Verify: from a tenant VM (`oak ssh`), `curl https://1.1.1.1` works (internet),
   `nc -z <other-tenant-app>.internal 5432` fails, and `dig <not-yours>.internal`
   is NXDOMAIN. The tunnel + health checks (host→VM) are unaffected (the host
   routes to every bridge).

## Out of scope
Per-tenant egress firewalling (tenants can still reach the public internet
freely), bandwidth/quota limits, and IPv6 (guests are IPv4-only).

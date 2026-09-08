# oakberryio — design

Self-hosted, Fly.io-inspired PaaS on one Ubuntu box. Firecracker microVMs, Go control plane, Cloudflare Tunnel ingress.

## Goals
- Host Iago's real apps (pigeon, prospects, nutrafluxo, ...) with `oak deploy`.
- One node now. Every table carries `node_id` so a second node is a store swap, not a rewrite.
- Docker image in, running microVM out. No buildpacks, no git-push.

## Non-goals (v1)
Multi-node, own edge proxy / Anycast, gossip state (Corrosion), metrics, machines-API parity, zero-downtime guarantees.

## Hardware
Ryzen 2700 (enable SVM in B450 BIOS), 16 GB RAM (~2 GB host, rest for VMs), 240 GB SSD (host + images + rootfs + volumes), 1 TB HDD (backups).

## Components
| Piece | Impl |
|---|---|
| `oakd` | Go daemon, systemd. API (HTTP over unix socket + tunneled HTTPS), scheduler, rootfs builder, VM supervisor, IPAM, DNS, tunnel config writer. |
| `oak` | Go CLI. `deploy`, `logs`, `status`, `secrets set`, `volumes create`, `ssh`. |
| `oak-init` | Static Go PID 1 inside the VM: mount rootfs/volume, set env, exec CMD, forward signals, serial-console logs. |
| Firecracker | Upstream binary + `vmlinux` built once. Jailer optional v1. |
| registry:2 | Host-only OCI registry the CLI pushes to. |
| cloudflared | One tunnel; `oakd` rewrites ingress rules per deploy. |
| SQLite | `/var/lib/oak/oak.db` — apps, machines, ips, volumes, secrets, releases. |

## Deploy flow
1. `oak deploy` reads `oak.toml` (name, build/image, env, services.internal_port, mounts, vm.memory_mb, vm.cpus, checks).
2. Builds with docker, pushes to `localhost:5000/<app>:<release>`.
3. `oakd` pulls, flattens layers → ext4 rootfs file (`images/<app>/<release>.ext4`).
4. Allocates tap + IP, writes Firecracker config, boots VM with `oak-init`.
5. Health check (TCP or HTTP from `oak.toml`) passes → old machine stopped → tunnel ingress updated → release marked active.
6. Failure → new machine killed, old untouched, exit non-zero with logs.

## Networking
- Bridge `oak0` 10.200.0.0/16, tap per machine, IPs from SQLite IPAM.
- nftables masquerade out. No inbound except via tunnel.
- `oakd` runs a DNS resolver on 10.200.0.1: `<app>.internal` → machine IPs. VMs get it as nameserver.
- Ingress: cloudflared ingress rule `<app>.<domain>` → `http://10.200.x.y:<port>`. TLS at Cloudflare.

## Storage
- `/var/lib/oak/{images,rootfs,volumes}` on SSD. Volumes = raw sparse files attached as `/dev/vdb`, mounted by `oak-init` at `mounts.destination`.
- `/var/lib/oak/backups` on HDD. Nightly systemd timer: fsfreeze-less copy of volume files (v1 accepts crash-consistent backups).
- Secrets: encrypted with an `age` key at `/etc/oak/key`, decrypted only into the VM boot env.
- Logs: serial console → `/var/log/oak/<app>/<machine>.log`, logrotate. `oak logs -f` tails.

## Ops
- `oakd` reconciles desired state from SQLite on start: every `machine.state = running` row gets booted.
- Crash of `oakd` kills VMs (Firecracker children). Acceptable v1; jailer + detached supervision later.
- `oak status` shows machines, IPs, mem/cpu from `/proc` of firecracker PIDs.

## Testing
- Unit: rootfs builder (layer flatten), IPAM, toml parsing, tunnel config rendering.
- Integration (on the box): `oak deploy` of a hello-world image end to end, health check, `curl` via tunnel, `oak logs`.
- One smoke script `scripts/smoke.sh` doing exactly that.

## Later (in order of likely pain)
1. Metrics endpoint.
2. Jailer + cgroups per VM.
3. Second node: replace SQLite with LiteFS/Corrosion, add WireGuard mesh, scheduler picks node.
4. Own edge proxy in front of tunnel.

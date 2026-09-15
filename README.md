# oakberryio

A small, self-hosted PaaS for your homelab — Fly.io-style deploys on your own
hardware. Push an app with an `oak.toml` and `oakberryio` builds it, boots it in
a [Firecracker](https://firecracker-microvm.github.io/) microVM, health-checks
it, and serves it over a Cloudflare Tunnel. Written in Go, no Kubernetes, no
cloud bill.

```sh
oak deploy          # build, boot a microVM, cut over when healthy
oak apps            # list your apps
oak logs myapp -f   # tail logs
oak ssh myapp       # shell into the running microVM
```

## What you get

- **Firecracker microVMs**, not containers — real kernel isolation per app.
- **`oak.toml` manifests** in the spirit of `fly.toml`: image or Dockerfile,
  env, ports, health checks, volumes, VM size, custom domains.
- **Zero-downtime deploys**: a new machine boots and passes its health check
  before traffic cuts over.
- **Cloudflare Tunnel ingress** — no ports open to the internet; every app gets
  `‹app›.‹your-domain›` plus any custom domains you list.
- **Internal DNS**: apps reach each other at `‹app›.internal`.
- **Persistent volumes** for stateful apps (e.g. Postgres).
- **`oak ssh`** into any running microVM over vsock.
- **Per-tenant network isolation**: each owner's apps get their own subnet and
  bridge, so one tenant's VM can't reach another's services or your databases.

The `oak` CLI runs on macOS and Linux. `oakd` (the daemon) and `oak-init` (the
guest init) run on a Linux box with KVM.

## Install the CLI

**Prebuilt binary** (macOS/Linux, amd64/arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/iagocavalcante/oakberryio/main/scripts/install.sh | sh
```

**With Go:**

```sh
go install github.com/iagocavalcante/oakberryio/cmd/oak@latest
```

Or grab an archive from the [releases page](https://github.com/iagocavalcante/oakberryio/releases).
Check your version with `oak --version`.

## Requirements for the box

A machine to run your apps on:

- Ubuntu Server 24.04 (or similar) with **KVM** — a CPU with SVM (AMD) or
  VT-x (Intel) enabled in BIOS, and `/dev/kvm` present.
- Docker (for building images and the local registry), and a Cloudflare account
  with a domain, for the tunnel.

Bring the box up with the bootstrap script and the host guide:

```sh
OAK_DOMAIN=apps.example.com ./scripts/host-setup.sh
```

Full walkthrough — BIOS, the Cloudflare Tunnel, DNS, and `/etc/oak/oakd.toml` —
is in [`docs/host.md`](docs/host.md).

## Quickstart

Once the box is up and `oakd` is running, from an app directory with an
`oak.toml`:

```sh
oak deploy            # build locally + push to the box's registry, then deploy
oak deploy --remote   # or build on the box (no local Docker needed)
oak apps              # list apps
oak status myapp      # machines, IPs, health
oak logs myapp -f     # tail logs
oak restart myapp
oak scale myapp --memory 1gb --cpus 2
oak destroy myapp -y  # alias: rm
```

A minimal `oak.toml`:

```toml
app = "hello"

[build]
dockerfile = "Dockerfile"

[[services]]
internal_port = 8080
check.path = "/healthz"   # omit for a plain TCP-connect check

[vm]
memory_mb = 256
cpus = 1
```

Runnable examples are in [`examples/`](examples/): a tiny Go web app
([`hello`](examples/hello)) and a stateful Postgres with a persistent volume
([`postgres`](examples/postgres), see [`docs/postgres.md`](docs/postgres.md)).

### `oak.toml` reference

| Key | Meaning |
|---|---|
| `app` | App name (its default hostname is `‹app›.‹OAK_DOMAIN›`). |
| `image` | Deploy a prebuilt public image ref instead of building. |
| `[build] dockerfile`, `[build.args]` | Build from a Dockerfile; `args` become `--build-arg`. |
| `domains` | Extra custom domains to route to this app. |
| `[env]` | Runtime environment variables. |
| `[[services]] internal_port`, `check.path` | Port to route to; optional HTTP health path (else TCP check). |
| `[[mounts]] volume`, `destination` | Attach a persistent volume at a path. |
| `[vm] memory_mb`, `cpus` | Machine size. |
| `[deploy] release_command` | A command to run once before cutover (e.g. DB migrations). |

## How it works

- **`oakd`** is the daemon on the box. It builds rootfs images, boots
  Firecracker microVMs, runs health checks, records everything in SQLite,
  serves an internal `.internal` DNS resolver, and rewrites the Cloudflare
  Tunnel ingress on every deploy. It listens on a trusted local unix socket
  (`/run/oak/oak.sock`) and, optionally, a token-guarded `127.0.0.1` port
  exposed through the tunnel.
- **`oak-init`** is PID 1 inside each microVM: it sets up the mounts and
  network, runs your app, reaps it, and powers the VM down cleanly.
- **`oak`** is the CLI. Locally it talks to `oakd` over the unix socket (no
  token); remotely it speaks HTTPS to `OAK_API` with a bearer token from
  `OAK_TOKEN` or `~/.oak/token`.

An optional web control panel with GitHub sign-in lives in the companion
[oak-panel](https://github.com/iagocavalcante/oak-panel) project.

## Security expectations

This is homelab-grade software. The `oakd` unix socket is a full trust
boundary — any local process that can reach it controls the box, so keep it
local (it is by default). The tunnel-exposed API is guarded by a bearer token
you set in `oakd.toml`. Per-tenant network isolation (see
[`docs/plans/2026-09-15-phase-d-network-isolation-design.md`](docs/plans/2026-09-15-phase-d-network-isolation-design.md))
keeps a tenant's VMs off other tenants' services, but treat multi-tenant use as
"trusted friends," not "the public internet." Don't commit secrets: `oakd.toml`
holds your API token and lives only on the box.

## Development

```sh
make build   # build oak, oakd, oak-init (Linux) + oak-darwin
make test    # go test ./...
make install BOX=oak@your-box   # build + deploy to a box over SSH
```

## Docs

- [`docs/host.md`](docs/host.md) — bring up a box, tunnel, DNS, deploy/rollout.
- [`docs/postgres.md`](docs/postgres.md) — stateful apps with volumes.
- [`docs/plans/`](docs/plans/) — design docs for each feature.

## License

[MIT](LICENSE) © Iago Cavalcante

# Host setup (Ubuntu Server 24.04)

Manual steps to prepare the box before `scripts/host-setup.sh` and before
`oakd` can run.

## 1. Enable virtualization in BIOS

Reboot into BIOS/UEFI setup and enable **SVM** (AMD) or **VT-x/VMX** (Intel).
Without this, `/dev/kvm` will not exist and Firecracker cannot boot microVMs.
`scripts/host-setup.sh` checks for both `svm`/`vmx` in `/proc/cpuinfo` and for
`/dev/kvm`, and refuses to continue if either is missing.

## 2. Run the bootstrap script

As root, with `OAK_DOMAIN` set to the domain apps will be served under:

```bash
OAK_DOMAIN=apps.example.com ./scripts/host-setup.sh
```

This is idempotent — safe to re-run. It installs Firecracker, the guest
kernel, the `oak0` bridge (`10.200.0.1/16`), nftables NAT/forwarding, a local
Docker registry on `127.0.0.1:5000`, `cloudflared`, and generates an `age`
key at `/etc/oak/key` for secrets encryption.

## 3. Authenticate and create the Cloudflare Tunnel

```bash
cloudflared tunnel login
cloudflared tunnel create oak
```

`tunnel create` prints a tunnel ID and writes credentials to
`~/.cloudflared/<tunnel-id>.json` (as root, `/root/.cloudflared/`). Note the
tunnel ID — it goes in `/etc/oak/oakd.toml` (`tunnel_id`).

Then install `cloudflared` as a systemd service reading the config file
`oakd` rewrites on every deploy (`tunnel_config` in `oakd.toml`, default
`/etc/cloudflared/config.yml`):

```bash
mkdir -p /etc/cloudflared
cat >/etc/cloudflared/config.yml <<EOF
tunnel: <tunnel-id>
credentials-file: /root/.cloudflared/<tunnel-id>.json
ingress:
  - service: http_status:404
EOF
cloudflared service install
systemctl enable --now cloudflared
```

`cloudflared service install` writes its own unit from the package, so
there's no `deploy/cloudflared.service` in this repo. `oakd` overwrites
`/etc/cloudflared/config.yml`'s `ingress:` list on every deploy and restarts
the service (see `internal/tunnel`); the placeholder ingress above is just
enough for the service to start before the first deploy.

## 4. Point DNS at the tunnel

In the Cloudflare dashboard for the zone backing `OAK_DOMAIN`, add a wildcard
CNAME so every app subdomain routes through the tunnel:

```
*.apps.example.com  CNAME  <tunnel-id>.cfargotunnel.com
```

`oakd` renders `cloudflared`'s ingress config per-app (see Task 8); this DNS
record just needs to exist once, up front.

## 5. `/etc/oak/oakd.toml`

Sample config (see Task 8 for the fields `oakd` reads):

```toml
data_dir = "/var/lib/oak"
registry = "localhost:5000"
domain = "apps.example.com"
tunnel_id = "uuid"
tunnel_config = "/etc/cloudflared/config.yml"
tunnel_creds = "/root/.cloudflared/uuid.json"
key_file = "/etc/oak/key"
socket = "/run/oak.sock"
log_dir = "/var/log/oak"
# api_token is required on every request to oakd's TCP listener
# (127.0.0.1:api_port, tunneled as oak.<domain>) as "Authorization: Bearer
# <api_token>"; the unix socket at `socket` needs no token since it's
# trusted local access. Generate one with `openssl rand -hex 32`.
api_token = "change-me"
api_port = 7000
```

## 6. Pushing images from the Mac

`oak deploy` runs `docker build`/`docker push` from your Mac against the
box's local registry at `localhost:5000`, so it needs to be reachable at that
address locally. Before deploying, forward the port over SSH:

```bash
ssh -L 5000:localhost:5000 <box>
```

Leave that session open (or run it with `-fN` to background it) for the
duration of the `docker push`.

## Build-time arguments

`oak.toml`'s `[build.args]` table is passed to `docker build` as
`--build-arg` flags (one per entry, sorted by key for a stable command), for
Dockerfiles that bake values in at build time via `ARG`:

```toml
[build.args]
NEXT_PUBLIC_API_URL = "https://api.apps.example.com"
```

This is separate from `[env]`, which sets runtime environment variables in
the running machine; `[build.args]` values only exist during `docker build`
and must be re-declared as `ARG` in the Dockerfile to be baked into the
image.

## Custom domains

An app's default hostname is `<app>.<OAK_DOMAIN>`. To also serve it under
one or more of your own domains, list them in `oak.toml`:

```toml
domains = ["misesnag.app", "www.misesnag.app"]
```

Each entry is routed to the same service as the default hostname -- it does
not need to be a subdomain of `OAK_DOMAIN`. For each custom domain you still
have to, in that domain's own DNS/Cloudflare zone:

1. Add a `CNAME` (or `A`/`AAAA` via Cloudflare proxy) record pointing at
   `<tunnel-id>.cfargotunnel.com`, same as the wildcard record in step 4
   above.
2. Make sure that zone has TLS coverage for the hostname (e.g. a Cloudflare
   Universal SSL certificate, or your own) -- `oakd` only adds the ingress
   rule, it doesn't provision certificates outside `OAK_DOMAIN`'s zone.

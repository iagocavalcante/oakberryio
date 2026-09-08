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
key_file = "/etc/oak/key"
socket = "/run/oak.sock"
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

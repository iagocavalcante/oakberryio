# Running Postgres on oak

`examples/postgres` deploys stock `postgres:16` as an oak app, using a
persistent volume for its data directory and `oak ssh` (not the public HTTP
tunnel) for access. This walks through provisioning it end to end.

## Why this works

Two features make a stateful database viable as a regular oak app:

- **Volumes persist across redeploys.** `oak.toml`'s `[[mounts]]` attaches a
  named volume to the container's data directory; a redeploy boots a new
  machine but reuses the same volume, so Postgres's data survives it.
- **`oak ssh` reaches services with no HTTP endpoint.** Postgres speaks the
  Postgres wire protocol, not HTTP, so it has no `check.path` and isn't
  reachable through the app's public tunnel. `internal/daemon/checker.go`
  falls back to a plain TCP-connect health check when `check.path` is empty,
  and `oak ssh <app>` opens an interactive shell inside the machine (over a
  Firecracker vsock device, not the network) for `psql` or anything else
  that needs to reach `localhost:5432` from inside the box.

## The PGDATA gotcha

`examples/postgres/oak.toml` sets:

```toml
[env]
PGDATA = "/var/lib/postgresql/data/pgdata"
```

`PGDATA` is a *subdirectory* of the mount (`/var/lib/postgresql/data`), not
the mount root. A freshly created volume is `mkfs.ext4`'d and so already
contains ext4's own `lost+found` directory; Postgres's `initdb` refuses to
run against a data directory that already has files in it. Pointing
`PGDATA` one level down sidesteps that entirely -- `lost+found` sits next to
`pgdata`, not inside it.

## Provisioning

From `examples/postgres`:

```bash
# 1. Create the volume Postgres's data directory will live on.
oak volumes create pg pgdata 10

# 2. Set the password postgres:16's entrypoint uses to initialize the
#    superuser on first boot. Never put this in oak.toml -- secrets are
#    encrypted at rest and injected at boot, oak.toml is not.
oak secrets set pg POSTGRES_PASSWORD=<a-real-password>

# 3. Build and deploy.
oak deploy

# 4. Open a shell inside the machine and connect locally.
oak ssh pg
# inside the machine:
psql -U postgres -c "create table t (id int); insert into t values (1);"
```

## Confirming persistence

```bash
oak deploy   # rebuilds and boots a fresh machine, same volume
oak ssh pg
psql -U postgres -c "table t;"   # the row from before is still there
```

The new machine mounts the same `pgdata` volume the old one used, so
Postgres picks up right where it left off.

## Reachability, by design

There is deliberately no way to reach this Postgres from outside the box:

- It's never on the Cloudflare tunnel -- only HTTP services with a tunnel
  route get one, and this app's only service has no `check.path` to serve
  behind one anyway.
- It's reachable from other machines on the box's internal `10.200.0.0/16`
  network (same as any other app), and from the host via `oak ssh`.

For anything that needs to reach it from outside the box, the supported path
today is `oak ssh` on the box itself (`ssh` into the host, then `oak ssh
pg`). A TLS/Postgres wire proxy or a TCP port-forward to the operator's
machine is out of scope for this design -- see
`docs/plans/2026-09-14-ssh-and-stateful-design.md`'s "Out of scope" section.

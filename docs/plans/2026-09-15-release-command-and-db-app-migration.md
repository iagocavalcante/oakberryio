# release_command + migrating a DB-backed app (iagocavalcante.com)

Migrate the Phoenix/Ecto site iagocavalcante.com to oak, database included.
Everything needed already exists except one capability: a deploy-time release
command (Ecto migrations). This plan adds it, then assembles the migration
from existing pieces.

## What already exists (verified)

- **Service discovery.** oakd runs a DNS server on 10.200.0.1:53 resolving
  `<app>.internal` to that app's running machine IPs; guests use it as their
  resolver (oak-init writes resolv.conf with DNS=10.200.0.1). So the app
  reaches its DB at `postgres-<name>.internal:5432` with a stable name.
- **Postgres on a persistent volume** (examples/postgres, verified: data
  survives redeploy).
- **Custom domains** (`domains = [...]`) — the apex `iagocavalcante.com` is
  covered by the zone's Universal SSL cert.
- **Secrets** (DATABASE_URL, SECRET_KEY_BASE) and **oak ssh**.

## The one new capability: release_command

fly.toml runs `release_command = '/app/bin/migrate'` before a new version
serves. oak has no equivalent. Add one.

### Config
`internal/appconfig`: add a `Deploy` struct with `ReleaseCommand string`,
TOML `[deploy] release_command = "..."`.

### Execution (internal/daemon/deploy.go)
In `Deploy`, after `InsertRelease` and before `bootFromRelease`, if
`cfg.Deploy.ReleaseCommand != ""`, run it to completion in a transient VM:

- Boot a microVM from the same release rootfs, with the app's full env
  (`buildGuestEnv`, so DATABASE_URL/secrets are present) and network (so
  `*.internal` resolves and the DB is reachable). No volumes, no health check,
  no tunnel route.
- Override the guest's argv with the release command run through a shell:
  `["/bin/sh", "-lc", <release_command>]` (so `/app/bin/migrate` and anything
  with args/pipes work).
- Wait for the VM to power off, then determine the command's exit status and
  gate the deploy on 0: on non-zero (or no clean exit) fail the deploy with
  the release VM's log tail, and do NOT boot the app.

### Getting the exit status out of the guest
The Firecracker VMM exit doesn't carry the child's exit code. oak-init already
prints a final line before power-off; make it a stable, unambiguous marker and
have oakd read the release VM's log for it:

- oak-init: change the final line to `oak-init: child-exit status=<N>` (a
  defined oakd<->oak-init contract; both are one codebase).
- oakd: after the release VM exits, read the last `oak-init: child-exit
  status=<N>` line from its log; N==0 is success. Absent/non-zero → fail.

This is stringly-typed but a documented internal contract; note the coupling
in comments. (A vsock guest->host report is the cleaner long-term channel;
out of scope now.)

### Reconcile
The release command runs only on `Deploy`, never on `Reconcile`/`Restart`
(migrations must not re-run when oakd restarts existing machines).

## The migration (assembly, mostly operational)

1. **Postgres app on oak**: deploy `examples/postgres` as app `iagocavalcante-db`
   with a volume; set `POSTGRES_PASSWORD`. Reachable at
   `iagocavalcante-db.internal:5432`.
2. **Data copy**: `pg_dump` the current Fly Postgres (needs the current
   DATABASE_URL or `fly proxy` to it) and restore into the oak Postgres via
   `oak ssh iagocavalcante-db -- psql`. A short maintenance window keeps it
   consistent.
3. **App**: deploy the Phoenix app as `iago` with
   `domains = ["iagocavalcante.com"]`, `internal_port = 8080`, a `/data/posts`
   volume, `[deploy] release_command = "/app/bin/migrate"`, and secrets
   `DATABASE_URL=ecto://…@iagocavalcante-db.internal:5432/…`,
   `SECRET_KEY_BASE`, `PHX_HOST=iagocavalcante.com`. The release command runs
   the Ecto migrations against the oak DB before cutover.
4. **/data/posts files**: copy the Fly volume's contents into the oak volume
   (tar over `oak ssh`, or restore from backup).
5. **DNS cutover**: point `iagocavalcante.com` (and `www` if used) at oak's
   tunnel; the apex is cert-covered. Reversible by pointing back at Fly.

## Prerequisites from the user (for the data step)
- Access to the current DB: the production `DATABASE_URL`, or `fly postgres`
  connect / `fly proxy` so I can `pg_dump`.
- `SECRET_KEY_BASE` and `DATABASE_URL` to set as oak secrets.
- The `/data/posts` contents (or confirmation the posts are in git and the
  volume is a cache).

## Out of scope
Zero-downtime DB cutover (a short maintenance window is fine for a blog);
vsock-based release exit reporting; multi-node.

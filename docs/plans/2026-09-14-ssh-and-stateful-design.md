# oak ssh + stateful Postgres — design (2026-09-14)

Two features that fit together: an interactive shell into a running microVM
(`oak ssh <app>`), and a proven Postgres-on-oak setup using the volumes that
already persist across redeploys. `oak ssh` is the access path for a DB, which
should not sit on the public HTTP tunnel.

## Feature 1: `oak ssh <app>`

Firecracker exposes a **vsock** device: a host Unix-domain socket bridged to
an AF_VSOCK socket inside the guest. Three parts.

### Guest agent (oak-init)
oak-init is already PID 1 supervising the app child, so it stays resident. Add
a goroutine started right after the child launches:

- Open an AF_VSOCK listener (`unix.AF_VSOCK`, `SockaddrVM{CID: VMADDR_CID_ANY,
  Port: 10000}`) via `golang.org/x/sys/unix` — no new dep for the listener.
- On each accepted connection: read one newline-terminated JSON header
  `{"cmd":[...],"rows":N,"cols":N}`. Allocate a PTY (`github.com/creack/pty`),
  spawn `cmd` (default `["/bin/sh"]`) with the app's env (from the mmds guest
  config, so `psql`/`PG*` are present), set the initial winsize, and
  `io.Copy` both directions between the vsock conn and the PTY master.
- If the command can't start (no shell in a scratch image), write a one-line
  error back and close, so the CLI can print "no shell in this image".
- The goroutine is best-effort: its failures are logged, never fatal to boot
  or to the app.

### Host bridge (oakd)
New endpoint `POST /apps/{name}/ssh`:

- Resolve the app's running machine and its vsock UDS path.
- `http.Hijacker` the connection, write `HTTP/1.1 200 OK\r\n\r\n`.
- Dial the machine's Firecracker vsock UDS, write `CONNECT 10000\n`, read the
  `OK <port>` line Firecracker returns (host-initiated vsock handshake).
- Bridge the hijacked client conn <-> the vsock conn until either closes. The
  client's JSON header and all PTY bytes flow straight through; oakd is a dumb
  pipe past the handshake.
- Bearer auth already wraps the TCP listener; the unix socket is trusted.

### Firecracker config (vm)
- Add `VsockUDS string` and `GuestCID uint32` to `vm.Spec`; in `BuildConfig`
  add a `firecracker.VsockDevice{ID:"vsock0", CID: guestCID, Path: vsockUDS}`.
- oakd sets `VsockUDS = <socketDir>/<id>_vsock.sock`, `GuestCID = 3` in both
  `bootFromRelease` (covers deploy/restart/scale) and `reconcileOne` (covers
  daemon restart). Remove a stale `<id>_vsock.sock` before Start, same as the
  api socket guard.

### CLI (`oak ssh <app> [-- cmd...]`)
- Speak HTTP/1.1 manually on a raw connection (like `docker exec`): dial the
  unix socket (or TCP), send the POST, read `200`, then the socket is the raw
  duplex stream. Send the JSON header, then pipe.
- Put the local terminal in raw mode (`golang.org/x/term`), send initial
  `rows`/`cols` from `term.GetSize`, copy stdin->conn and conn->stdout,
  restore the terminal on exit.
- **Scope:** target the unix socket first (run on the box: `sudo oak ssh pg`).
  Raw duplex over the cloudflared HTTP tunnel is unreliable, so remote-over-
  tunnel is a later item. Live resize (SIGWINCH) is v2; v1 sets size once.

### New deps
`github.com/creack/pty` (guest PTY), `golang.org/x/term` (client raw mode).
Both small and standard; PTY handling by hand is error-prone, so the dep is
warranted.

## Feature 2: stateful Postgres

### Health check: TCP when no HTTP path
The checker is HTTP-only (`Healthy(ctx, ip, port, path)` does a GET). Postgres
has no HTTP endpoint. Change: when a service's `check.path` is empty, do a
**TCP dial** to `internal_port` and treat a successful connect as healthy.
This gates cutover on Postgres actually accepting connections. Update the
Checker implementation and `awaitHealthy`'s use of it accordingly.

### examples/postgres
- `Dockerfile`: `FROM postgres:16` (so the existing build+push+rootfs flow
  works unchanged; no deploy-path change for prebuilt images).
- `oak.toml`:
  ```
  app = "pg"
  [build]
  dockerfile = "Dockerfile"
  [env]
  PGDATA = "/var/lib/postgresql/data/pgdata"   # subdir avoids ext4 lost+found
  [[mounts]]
  volume = "pgdata"
  destination = "/var/lib/postgresql/data"
  [[services]]
  internal_port = 5432
  # no check.path -> TCP check
  [vm]
  memory_mb = 512
  ```
- `POSTGRES_PASSWORD` set out of band via `oak secrets set pg
  POSTGRES_PASSWORD=...` (injected at boot).

### The lost+found gotcha
A freshly `mkfs.ext4` volume contains `lost+found`, so Postgres sees a
non-empty data dir and refuses `initdb`. Setting `PGDATA` to a subdirectory of
the mount sidesteps it. Documented and baked into the example.

### docs/postgres.md
Workflow: `oak volumes create pg pgdata 10` -> `oak secrets set pg
POSTGRES_PASSWORD=...` -> `oak deploy` -> `oak ssh pg` then `psql` -> redeploy
and confirm data persists. Note the DB is reachable only inside the box's
network and via `oak ssh`, by design.

## Verification on hardware (192.168.1.8)
1. Build oakd/oak/oak-init, install, restart oakd.
2. `oak ssh hello` -> expect "no shell in image" (scratch), proving the path
   and the graceful error.
3. Create volume + secret, deploy examples/postgres, `oak ssh pg`, run psql:
   create a table, insert a row.
4. `oak deploy` pg again; `oak ssh pg`, confirm the row survived (volume
   persistence end to end).

## Out of scope (stated)
`oak ssh` over the tunnel, live window-resize, TCP port-forward to the Mac,
a TLS/Postgres wire proxy, multi-replica DBs.

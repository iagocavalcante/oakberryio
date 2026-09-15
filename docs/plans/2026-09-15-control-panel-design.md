# oak control panel (read-only, live)

A small web panel to see the box's health at a glance: host CPU/mem/disk,
which microVMs are running, and per-VM CPU/memory. Read-only, live snapshots
(no history yet). Served by oakd at `oak.<domain>/dashboard`.

Deliberately excludes friends-deploy / multi-tenancy (a separate effort with
its own auth + network-isolation + quota design); this panel is admin-only.

## Metrics collection (oakd, Linux)

A background sampler goroutine (started in daemon Run) recomputes a snapshot
every ~2s and stores it behind a mutex; `GET /metrics` returns the latest.
Sampling in the background (not per-request) keeps the endpoint cheap and lets
CPU% be a real delta between samples.

Host (from `/proc`, `statfs`):
- `cpu_pct`  — from `/proc/stat` deltas between samples.
- `mem_used_bytes`, `mem_total_bytes` — `/proc/meminfo` (MemTotal - MemAvailable).
- `disk_used_bytes`, `disk_total_bytes` — `statfs` on the data dir.
- `load1` — `/proc/loadavg`.
- `uptime_secs` — `/proc/uptime`.
- `vm_count` — running machines from the store.

Per running machine (the store gives id/app/ip/pid/state):
- `cpu_pct` — `/proc/<pid>/stat` utime+stime deltas between samples.
- `mem_bytes` — RSS from `/proc/<pid>/status` (VmRSS) for the Firecracker PID.
- A machine whose PID is gone reports zero / is shown as not-running.

Put the `/proc` reads in `internal/metrics` with a `_linux.go` real impl and a
`_other.go` stub (returning a zero snapshot) so the package and its tests
build on darwin, mirroring how internal/vm and the daemon already split.

## API

- `GET /metrics` — bearer-authed (TCP) / trusted (socket), JSON:
  `{ "host": {…}, "machines": [ {id, app, ip, pid, state, cpu_pct, mem_bytes}, … ] }`.
- `GET /dashboard` — serves the HTML page. This route is EXEMPT from
  AuthMiddleware (it carries no data; a browser navigation can't send a bearer
  header). Add a small exemption in AuthMiddleware for exactly this path.

## The page (served by oakd)

One self-contained HTML file (inline CSS+JS, no framework, no CDN), embedded
in the oakd binary via `go:embed`:
- On load, read the API token from `localStorage`; if absent, prompt for it and
  store it. All `fetch('/metrics')` calls send `Authorization: Bearer <token>`.
- Poll `/metrics` every 3s. Render:
  - Host row: CPU%, memory used/total, disk used/total as labeled bars, plus
    load1, uptime, and running-VM count.
  - A table of running microVMs: app, machine id (short), IP, CPU%, memory.
- Theme-aware (respects `prefers-color-scheme`), works at phone width, shows a
  clear "enter token" state and an error state if `/metrics` 401s.

Same origin as the API, so no CORS. Reachable at `oak.<domain>/dashboard`
(that hostname already routes to oakd through the tunnel).

## Auth / security

- The data endpoint stays behind the existing bearer token. The token lives in
  the operator's browser localStorage only.
- Hardening (later, with friends-deploy): put `oak.<domain>` behind Cloudflare
  Access and have oakd trust the Access identity, dropping the manual token.

## Out of scope
Historical/time-series metrics and charts; per-tenant views; any write actions
from the panel; friends-deploy and multi-tenancy.

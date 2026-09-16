# Migrate non-media workloads from tron to the oak box

Date: 2026-09-15. Source: tron (`192.168.1.12`, 8 cores / 15 GB / 59 GB free,
Docker + per-project cloudflared tunnels). Target: oak (`192.168.1.8`, 16 cores /
15 GB / 201 GB free, oakd + one `oak` tunnel).

## Ground rules

- One oak app per container. Compose stacks become N oak apps talking over
  `<app>.internal`. Databases become oak apps with a persistent volume
  (see `docs/postgres.md`).
- Deploy first **without** `domains` so the app comes up at
  `<app>.iagocavalcante.com`; verify; only then add `domains` and redeploy.
  Adding `domains` is the cutover (oakd rewrites ingress and routes DNS).
  **Caveat:** if the app name equals the public hostname's first label
  (e.g. `fitlock` → `fitlock.iagocavalcante.com`), the first deploy *is* the
  cutover, because oakd routes the default hostname's DNS to the oak tunnel.
  For those apps stage under a different name or accept the immediate switch.
- Data moves with `pg_dump | psql` (all DBs are < 50 MB) after the target app is
  up, then a final dump at cutover. Stop the tron container after cutover, keep
  the tron volume for a week, then remove.
- Secrets come from tron's `.env` files into `[env]` in a gitignored
  `oak.prod.toml`, or from the panel; never commit them.
- Apps deployed by self-hosted GitHub runners on tron switch to a workflow step
  that runs `oak deploy` against `OAK_API` with a token secret; the runner
  service on tron is then disabled.

## Already on oak (no action)

| App | Note |
|---|---|
| iagocavalcante.com / www | `iago` app; tron container exited, tunnel entry on tron is dead. Remove the stale `media-config.yml` lines when the media tunnel is next edited. |
| misesnag.app web | `misesnag` app (10.200.0.6:3000). **API, admin and DB are still on tron** (see below). |
| pg, iagocavalcante-db, hello | oak-native. |

## Stays on tron (out of scope or not an HTTP app)

| Workload | Why |
|---|---|
| media-stack, jellyfin, jellyfin-alexa, ptbr-subgen, media tunnel | media, explicitly excluded |
| minecraft (25565/19132 direct LAN ports) | oak only routes HTTP via tunnel |
| age-of-amazon (`game.iagocavalcante.com`, native gateway + caddy user units) | not containerised; websockets/UDP; revisit separately |
| EF-Nutry GitHub runner, openclaw-gateway, tailscale, nordvpn | host infra |
| deploy-* (pigeon dev stack, mongo 0.3 MB, no public hostname) | dev scratch, not production |
| `dev-end.iagocavalcante.com` nutrafluxo-dev tunnel | tunnel has 0 connections: already dead. Decide separately whether nutrafluxo goes to oak at all. |

## To migrate, in order (easy → risky)

| # | Public host | tron pieces | oak apps | Source | Deploy path today |
|---|---|---|---|---|---|
| 1 | fitlock.iagocavalcante.com | nginx static (`~/apps/fitlock`) | `fitlock` | Mac `fitlock/web` | **DONE 2026-09-15** — live on oak, tron container stopped, ingress line removed (media tunnel restart pending sudo) |
| 2 | trainergymai.app / www | trainer-gym-landing | `trainer-gym-landing` | GH `iagocavalcante/trainer-gym-ai` | **STAGED** on oak with domains, CI switched to `oak deploy --remote` (v0.1.4), tron runner disabled. **DNS switch pending Cloudflare token** (trainergymai.app zone). |
| 3 | prospects.iagocavalcante.com | prospecting nginx + prospector api + outreach + pgvector pg17 | `prospects`, `prospector`, `outreach`, `prospector-db` | tron `~/prospector-build` (deployed copy; Mac repo differs) | **DONE 2026-09-16** — data restored (51 companies/leads), Access verified, tron stopped, backup cron removed. |
| 4 | beatduel.iagocavalcante.com | beatduel-relay + own tunnel + watchdog cron | `beatduel` | tron `~/Workspaces/beatduel-relay` | **DONE 2026-09-16** — tron container stopped, watchdog cron and tunnel process removed. |
| 5 | oakberry.iagocavalcante.com | oakberry (Elixir) + pg16 + uploads volume + own tunnel | `oakberry`, `oakberry-db` | tron `~/apps/oakberry` (deployed copy) | **DONE 2026-09-16** — 19 tables restored, migrations ran, uploads volume (was empty), tron stack stopped. |
| 6 | agendare.iagocavalcante.com | agendare app + pg18 + own tunnel | `agendare`, `agendare-db` | tron `~/agendare/server` | **DONE 2026-09-16** — 24 tables restored, tron stack stopped. |
| 7 | leaftok-api.iagocavalcante.com | leaftok-api (Elixir, host net) + pg17 | `leaftok-api`, `leaftok-db` | GH `LeafTok/api` | **DONE 2026-09-16** — 12 tables restored; CI deploys via `oak deploy --remote`; tron containers stopped, runner disabled. Needed oak-init HOME fix + CLI v0.1.2–v0.1.4. |
| 8 | agendflow.com.br / www / api. | agendflow-client (nginx) + agendflow-api (Supabase DB, no data to move) | `agendflow-client`, `agendflow-api` | GH `Agendflow/*` | **STAGING** on oak with domains; CI not yet switched. **DNS switch pending Cloudflare token** (agendflow.com.br zone). |
| 9 | api.misesnag.app, admin.misesnag.app | misesnag-api, misesnag-admin, misesnag-db (14 MB, 36 tables) + `misesnag-tunnel` user unit + daily blog cron | `misesnag-api`, `misesnag-admin`, `misesnag-db` | tron `~/apps/misesnag` @ c67cdd6 | **STAGED & VERIFIED** on oak (`misesnag-api.iagocavalcante.com`, `misesnag-admin.…`). Data restored, readiness 200. **DNS switch pending Cloudflare token** (misesnag.app zone). |
| 10 | (none yet) nutrafluxo | api + ai + pg16 (40 MB), nightly backup cron | `nutrafluxo-api`, `nutrafluxo-ai`, `nutrafluxo-db` | Mac `nutrafluxo` | only if it should be public again |

Each step: write `oak.toml` in the app repo → `oak deploy` → verify on
`<app>.iagocavalcante.com` (and `.internal` from a sibling VM) → migrate data →
add `domains` + redeploy → stop tron container → delete tron tunnel ingress
line / disable its cloudflared unit → move cron jobs.

## DNS: how cutover actually works

oakd does not create DNS records. `*.iagocavalcante.com` is NOT a wildcard
to the oak tunnel; each host has its own CNAME. Switch a host with
`cloudflared tunnel --config /dev/null route dns -f b1e1e3ba-e94e-4452-a57e-999457238e19 <host>`
on tron (its cert.pem covers only the iagocavalcante.com zone). Other zones
(trainergymai.app, misesnag.app, agendflow.com.br) need a Cloudflare API
token with DNS edit; two junk records
`trainergymai.app.iagocavalcante.com` / `www.trainergymai.app.iagocavalcante.com`
were created by mistake and need deleting with that token too.

## Blocked: the box itself is unstable (2026-09-16)

The box powered off abruptly twice within half an hour, the second time ten
minutes after a manual power-on, with no shutdown sequence, panic, OOM or
thermal line in the journal — it simply stops. tron, in the same house, was
unaffected. **Rolled back on 2026-09-16:** misesnag (apex and admin) and trainergymai
(apex and www) were pointed back at tron's tunnels in the Cloudflare
dashboard, and trainer-gym's CI was reverted to the tron self-hosted runner.
Their oak copies stay staged but take no traffic. agendflow was never moved.
fitlock was rolled back the same way. oakberryio.iagocavalcante.com now fails
over to a static copy on tron (landing, `install.sh`, `box.sh` only) behind a
dedicated tunnel `oakberryio-tron`; it must be re-synced if those files
change, and pointed back at the oak tunnel once the box is healthy.
Until the hardware is diagnosed, **do not move any DNS back to oak**. tron's ops monitor now alerts on the box within about
four minutes.

## Findings that outlived the migration

- **`oak` gaps fixed along the way** (all released): `oak-init` now seeds
  `HOME` like Docker does (v0.1.2 — leaftok-api's Python runtime crashed
  without it); `oak deploy --remote` ships the Dockerfile even when
  `.dockerignore` lists it and honours allowlist-style exceptions
  (v0.1.2/v0.1.4); `ok <machine-id>` is emitted *before* the cloudflared
  restart and the CLI treats a stream reset after it as success (v0.1.3 —
  every CI deploy failed otherwise); `install.sh` resolves the latest release
  through github.com's redirect rather than `api.github.com`, whose 60/hour
  unauthenticated budget shared CI IPs exhaust.
- **Remote build contexts must be lean.** The tunnel rejects a body over
  ~100 MB with a Cloudflare 413. trainer-gym-ai's monorepo needed an
  allowlist `.dockerignore`; expect the same for any other monorepo.
- **A rootfs is image size + 256 MB.** Anything that writes real data needs a
  volume. `misesnag-api` writes whole videos through `os.tmpdir()`, so it
  mounts a 20 GB `scratch` volume with `TMPDIR=/scratch`. Its readiness check
  still `statfs('/')`, so it reports `disk usage is 91%` (the rootfs) forever
  — cosmetic today, but the honest fix is for that check to measure the
  filesystem the app writes to.
- **misesnag's off-site backup is not running and has not been since
  2026-08-25**, on tron, before any of this. `scripts/backup-db.sh` has no
  crontab entry there; the newest dump in `~/backups/misesnag` is from
  2026-08-25 and the API's readiness has been warning
  "offsite backup heartbeat file is missing" on tron too. Porting it to oak
  means resolving the DB machine's IP (it changes per deploy: ask oakd's
  resolver at `10.200.0.1` for `misesnag-db.internal`) and running `pg_dump`
  from a throwaway container. Worth doing, but it is a pre-existing gap, not
  something the migration broke.
- **Every deploy blips every app.** `internal/tunnel` applies ingress by
  restarting cloudflared, so all 24 apps on the box drop connections for a
  second or two on any deploy (a CI deploy mid-verification made every
  unrelated host return Cloudflare 530 for ~15s). Harmless at one app, worth
  fixing now that the whole estate shares one tunnel; the upgrade path the
  code already names is Cloudflare's remote-managed tunnel config API, which
  needs the same API token the DNS cutover is waiting on.
- oak's own `oak-backup.timer` snapshots `oak.db` and every volume image
  nightly to `/var/lib/oak/backups`, 7-day retention, **on the same box**.
  That is not an off-site backup for any app that needs one.

## Still on tron, by decision or by dependency

Running there now: the media stack, minecraft, age-of-amazon, the pigeon
`deploy-*` dev stack, nutrafluxo (api + ai + pg, not publicly routed since
the `dev-end` tunnel died; decide separately whether it moves), and the four
staged-but-not-cut-over production containers (`agendflow-api`,
`agendflow-client`, `misesnag-*`, `trainer-gym-landing`) which keep serving
until their DNS moves.

Cron still on tron: `docker-cache-prune.sh`, `misesnag-daily-blog.sh` (git +
OpenAI + docker, independent of where the app runs), and nutrafluxo's DB
backup.

## After all steps

- Tron tunnels `misesnag`, `oakberry`, `beatduel`, `agendare`, `nutrafluxo-dev`
  become empty: delete them in Cloudflare. `media-stack` keeps only media hosts.
- Disable the five per-project GH runner user units on tron.
- Rotate any secret that was copied over a shell session.

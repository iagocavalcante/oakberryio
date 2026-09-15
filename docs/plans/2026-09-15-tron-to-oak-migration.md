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
| 2 | trainergymai.app / www | trainer-gym-landing | `trainer-gym-landing` | GH `iagocavalcante/trainer-gym-ai` | runner on tron |
| 3 | prospects.iagocavalcante.com | prospecting nginx + prospector api + outreach + pgvector pg17 | `prospects-web`, `prospector`, `outreach`, `prospector-db` (pgvector image, volume) | Mac `prospector` + tron `~/prospecting` | manual; cron backup on tron |
| 4 | beatduel.iagocavalcante.com | beatduel-relay + own tunnel + watchdog cron | `beatduel-relay` | tron `~/Workspaces/beatduel-relay` | manual |
| 5 | oakberry.iagocavalcante.com | oakberry (Elixir) + pg16 + uploads volume + own tunnel | `oakberry`, `oakberry-db` | Mac `oakberry` | manual |
| 6 | agendare.iagocavalcante.com | agendare app + pg18 + own tunnel | `agendare`, `agendare-db` | Mac `agendare` | manual |
| 7 | leaftok-api.iagocavalcante.com | leaftok-api (Elixir, host net) + pg17 | `leaftok-api`, `leaftok-db` | GH `LeafTok/api` | runner on tron |
| 8 | agendflow.com.br / www / api. | agendflow-client (nginx) + agendflow-api | `agendflow-client`, `agendflow-api` (+ DB? check api env) | GH `Agendflow/*` | 2 runners on tron |
| 9 | api.misesnag.app, admin.misesnag.app | misesnag-api, misesnag-admin, misesnag-db (35 MB) + `misesnag-tunnel` user unit + daily blog cron | `misesnag-api`, `misesnag-admin`, `misesnag-db`; repoint the existing `misesnag` web at `misesnag-api.internal` | tron `~/apps/misesnag*` | manual, several release dirs |
| 10 | (none yet) nutrafluxo | api + ai + pg16 (40 MB), nightly backup cron | `nutrafluxo-api`, `nutrafluxo-ai`, `nutrafluxo-db` | Mac `nutrafluxo` | only if it should be public again |

Each step: write `oak.toml` in the app repo → `oak deploy` → verify on
`<app>.iagocavalcante.com` (and `.internal` from a sibling VM) → migrate data →
add `domains` + redeploy → stop tron container → delete tron tunnel ingress
line / disable its cloudflared unit → move cron jobs.

## After all steps

- Tron tunnels `misesnag`, `oakberry`, `beatduel`, `agendare`, `nutrafluxo-dev`
  become empty: delete them in Cloudflare. `media-stack` keeps only media hosts.
- Disable the five per-project GH runner user units on tron.
- Rotate any secret that was copied over a shell session.

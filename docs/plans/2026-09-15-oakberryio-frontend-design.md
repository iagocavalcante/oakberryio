# oakberryio frontend — Phoenix/LiveView control panel with GitHub auth

A web control panel where an allowlisted set of GitHub users can sign in,
see the box, and deploy/manage their own apps. Phoenix + LiveView, run as a
host service next to oakd. Supersedes the embedded read-only `/dashboard`
(that stays as a fallback).

## Decisions (chosen)
- **Host service.** Phoenix release + systemd on the box, talking to oakd over
  the trusted unix socket (`/run/oak.sock`) — no token in the app. Served
  publicly through the tunnel at `panel.iagocavalcante.com`. The control plane
  must not depend on the VMs it manages.
- **Per-user ownership.** Each user manages only the apps they deployed; the
  owner (Iago) is an admin who sees/does everything.
- **Deploy by public image ref (MVP).** app name + public Docker image + port
  + optional env. No build pipeline in v1.

## The security boundary (read this first)
- oakd stays **localhost-only** (127.0.0.1 + unix socket). Users NEVER talk to
  oakd directly. The Phoenix app is the only oakd client and the sole place
  authorization is enforced: it knows the GitHub identity (from the session)
  and calls oakd on the user's behalf, scoping every action to apps that user
  owns (admin bypasses).
- **Ownership is control-plane only.** It does NOT isolate the network: a
  deployed VM sits on `oak0` (10.200.0.0/16) and can reach `*.internal`,
  including your databases. v1 ships with this documented for a *trusted*
  allowlist. Data-plane isolation (per-tenant nftables on oak0, or per-tenant
  subnets, blocking cross-tenant + DB reach) is a REQUIRED follow-up before
  onboarding anyone not fully trusted. Tracked as "Phase D".
- GitHub OAuth + an explicit login allowlist (config/env). Non-allowlisted
  logins are rejected after OAuth, before any session is created.

## oakd changes (Phase A — Go, this repo)
Ownership has to live in oakd (the frontend can be bypassed only by someone
with host access, but the store is the source of truth):
- `apps` table: add `owner TEXT NOT NULL DEFAULT ''` (the GitHub login).
- Deploy: accept an `owner` on first create (new field on the deploy request,
  or a small `PUT /apps/{name}/owner` the frontend calls before first deploy);
  never change owner on redeploy.
- Listings (`GET /apps`, `GET /metrics`, machines) include the owner so the
  frontend can filter/enforce.
- No new *enforcement* in oakd itself (it trusts its local clients); oakd just
  records and reports ownership. Enforcement is the frontend's job, and oakd's
  localhost-only binding is what makes that safe.

## Frontend (Phase B/C — new Elixir app `oak-panel`)
Phoenix 1.7 + LiveView. No Ecto/DB needed for MVP (state lives in oakd);
sessions in a signed cookie.
- **Auth:** `assent` (or ueberauth_github) GitHub OAuth. On callback, check the
  login against `PANEL_ALLOWLIST` (comma-separated); reject otherwise. Store
  `{login, name, avatar, is_admin}` in the session. `is_admin` = login ==
  `PANEL_ADMIN`.
- **oakd client:** an Elixir module speaking oakd's HTTP API over the unix
  socket (Finch/Mint support unix sockets via a custom conn, or `:httpc` with
  a unix transport; pick the cleanest). Wraps: list apps (with owner), metrics,
  deploy, restart, destroy, logs, machines.
- **LiveViews:**
  - `/` dashboard — host gauges + running-VM table (from `/metrics`), live
    (LiveView pushes every ~3s). Admin sees all VMs; a user sees theirs.
  - `/apps` — the user's apps (owner == login; admin sees all), status per app.
  - `/apps/new` — deploy form: name, image, internal_port, env pairs →
    validates, sets owner = login, calls oakd deploy, streams progress.
  - `/apps/:name` — detail: machines, live logs (stream oakd logs), Restart and
    Destroy buttons (owner/admin only), Scale (later).
- **Deploy path:** the panel calls oakd's existing deploy with an image ref
  (oakd pulls it from the public registry via BuildRootfs). App name is
  namespaced to avoid collisions (e.g. `<login>-<name>` or a validated global
  name with an owner check).

## Serving it (Phase C)
- Build the Phoenix release (on the box, natively — same lesson as iago).
- systemd unit `oak-panel.service` on the box; binds `127.0.0.1:<port>`.
- Tunnel: add `panel.iagocavalcante.com -> http://127.0.0.1:<port>` to
  cloudflared's ingress. Since oakd owns the ingress file, either teach oakd to
  keep a static extra route for the panel, or add it to the base ingress oakd
  preserves. Decide during Phase C.
- Harden with Cloudflare Access in front of `panel.` too (defense in depth over
  the in-app allowlist).

## Phasing
- **A — oakd ownership** (Go, small): owner column + deploy/create sets it +
  listings expose it. Foundation; do first.
- **B — panel scaffold**: Phoenix app, GitHub OAuth + allowlist, oakd
  socket client, read-only dashboard + my-apps.
- **C — deploy & manage**: deploy form (image ref), restart/destroy scoped to
  owner, live logs; host-service + tunnel + Access.
- **D — data-plane isolation** (required before untrusted users): per-tenant
  nftables/subnets on oak0 so a tenant VM can't reach other tenants or the
  host's internal services.

## Out of scope (MVP)
Build-from-repo, per-user quotas (Phase A can add columns later), scale from
the UI, billing, and D's network isolation (flagged as required-before-
untrusted, not part of MVP).

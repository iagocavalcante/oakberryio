# Panel admin "manage users" view

Let the admin add/remove allowlisted GitHub logins from the panel UI instead
of editing PANEL_ALLOWLIST + recreating the container.

## Storage
The allowlist becomes runtime-mutable, so it can't be a container env var.
Keep it panel-owned (auth is a panel concern; oakd unchanged): a JSON file on
a persistent bind-mounted volume (`DATA_DIR`, default `/data`, mounted from
`/var/lib/oak-panel` on the box). An Agent/GenServer holds it in memory and
persists atomically (write-temp-then-rename) on each change.

- **Seed**: on first boot, if `<DATA_DIR>/allowlist.json` is absent, seed it
  from the `PANEL_ALLOWLIST` env (comma-separated). After that the file is
  authoritative; the env is bootstrap-only.
- **Admin**: `PANEL_ADMIN` is always allowed, always admin, never stored in
  the file and never removable.

## Auth change
`allowed?(login)` = `login == PANEL_ADMIN` OR `login in Allowlist.list()`.
The OAuth callback uses this instead of the env list. `admin?/2` unchanged.

## Admin view (`/users`, admin-only)
- List current allowlisted logins (+ show the admin, marked, non-removable).
- Add a login (validate GitHub-username shape).
- Remove a login (not the admin, and ideally not yourself).
- Guarded by `is_admin` both in the router/mount and server-side in each event.

## Deploy delta
Panel container gains `-v /var/lib/oak-panel:/data` (create the host dir).
First boot seeds from the current `PANEL_ALLOWLIST=iagocavalcante,lubien,
franknfjr`, so no manual migration.

## Out of scope
Per-user roles beyond admin/non-admin, quotas, audit log.

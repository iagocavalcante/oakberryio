# oak CLI parity — design (2026-09-14)

Add four Fly-like operator commands: `restart`, `destroy`/`rm`,
`secrets list`/`unset`, and `scale` (VM resize only). First slice of a
larger "Fly for a homelab" effort; replica-count scaling, scale-to-zero,
zero-downtime strategies, fly.toml ingestion and `ssh` are explicitly out of
scope here.

## Shared foundation: `bootFromRelease`

`Deploy` today does two things in sequence:

1. Build + push image, `BuildRootfs`, `InsertRelease` (the release-producing
   half).
2. Alloc a machine, build the VM spec/guest, `Start`, health-check, mark
   running, `watchMachine`, `stopOldMachines`, `applyTunnel` (the boot half).

Extract the boot half into:

```
func (d *Deployer) bootFromRelease(ctx context.Context, cfg *appconfig.Config,
    rel store.Release, progress func(string)) (string, error)
```

It rebuilds the machine from `cfg` (VM size, env, services) and `rel`
(rootfs, entrypoint, cmd, env, workdir — decoded the same way `reconcileOne`
already does). `Deploy` calls it after `InsertRelease`. `restart` and `scale`
call it with the app's latest existing release, so they reboot with no
rebuild.

This inherits Deploy's health-gated cutover: the new machine boots and passes
its health check before `stopOldMachines` tears down the previous one, so
restart/scale are near-zero-downtime swaps, not stop-then-start gaps.

New store method: `LatestRelease(app string) (Release, error)` — most recent
release row for the app.

## Commands

### `oak restart <app>`
- CLI → `POST /apps/{name}/restart` → `handleRestart` → `Deployer.Restart`.
- `Restart`: load stored config, `LatestRelease(app)`, `bootFromRelease`.
- Errors if the app has no release yet.

### `oak scale <app> [--memory 1gb] [--cpus 2]`
- At least one of `--memory`/`--cpus` required.
- Memory parsing: `1gb`/`2GB` → 1024/2048; `512mb` → 512; bare integer = MB.
  Reject anything else.
- CLI → `POST /apps/{name}/scale` `{memory_mb, cpus}` (0 = leave unchanged)
  → `handleScale` → `Deployer.Scale`.
- `Scale`: load stored config, update `cfg.VM.MemoryMB`/`CPUs` for the
  non-zero fields, `UpsertApp` to persist, `LatestRelease`, `bootFromRelease`.
- Reboots the app (health-gated cutover). Brief overlap, same as a deploy.

### `oak destroy <app>` (alias `rm`), requires `-y`
- CLI refuses without `-y`; with it → `DELETE /apps/{name}` → `handleDestroy`
  → `Deployer.Destroy`.
- `Destroy`:
  1. Stop every machine for the app (`handle.Stop` if held, then
     `DeleteMachine`) — same shape as `stopOldMachines` with no survivor.
  2. Delete rows in FK order: machines, releases, secrets, volumes, then the
     app row.
  3. Remove on-disk artifacts: `<DataDir>/images/<app>/`,
     `<LogDir>/<app>/`, `<DataDir>/volumes/<app>-*.img`.
  4. `applyTunnel` to drop the app's ingress route.
- Registry blobs (`localhost:5000/<app>:*`) are left in place — registry GC
  is a separate concern; marked with a `ponytail:` note.
- New store methods: `DeleteApp`, `DeleteReleasesForApp`,
  `DeleteSecretsForApp`, `DeleteVolumesForApp`. `Destroy` wraps the row
  deletes so a mid-way failure doesn't leave a half-deleted app.

### `oak secrets list <app>` / `oak secrets unset <app> KEY...`
- `list` → `GET /apps/{name}/secrets` → sorted key names only, never
  ciphertext or plaintext. Client `SecretKeys(app) []string`.
- `unset` → `DELETE /apps/{name}/secrets` `{keys:[...]}` → `Store.DeleteSecret`
  per key.
- Both `set` and `unset` print `run: oak restart <app> to apply` — secrets
  inject into the guest env at boot, so a running machine doesn't pick up
  changes until it reboots. No auto-restart.

## API surface added

```
POST   /apps/{name}/restart
POST   /apps/{name}/scale       {memory_mb, cpus}
DELETE /apps/{name}             (destroy)
GET    /apps/{name}/secrets     -> ["KEY1","KEY2"]
DELETE /apps/{name}/secrets     {keys:[...]}
```

All share the existing name validation and the unix-socket/bearer-TCP split.

## Testing

- `appconfig`/CLI: memory-string parser table test (`1gb`, `512mb`, `2GB`,
  `1024`, and rejects `nonsense`, empty, negative).
- `store`: `LatestRelease` returns the newest of several; `DeleteSecret` and
  the `Delete*ForApp` methods remove only the target app's rows; a full
  cascade delete leaves no orphan rows.
- `daemon`: `Destroy` removes rows + files and drops the tunnel route;
  `Restart`/`Scale` boot a fresh machine from the latest release and supersede
  the old one; `handleScale` rejects a request with both fields zero.
- Keep the existing daemon test style (fakes for Runtime/Checker).

## Out of scope (stated, so it isn't silently assumed)

Replica count > 1, scale-to-zero, bluegreen/rolling strategies, fly.toml
ingestion, `oak ssh`, registry GC.

// Package daemon implements oakd: the deploy orchestrator, health checker,
// HTTP API and process wiring. It depends only on cross-platform packages
// directly (internal/rootfs, internal/vm's pure Spec/MACFromIP, internal/
// mmds, internal/store, internal/tunnel, internal/secrets, internal/
// appconfig) so it builds and its tests run on darwin. The only
// platform-specific piece -- actually pulling images, formatting ext4, and
// booting Firecracker -- sits behind the Runtime interface, implemented for
// real in runtime_linux.go and stubbed in runtime_other.go.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"

	"github.com/iagocavalcante/oakberryio/internal/appconfig"
	"github.com/iagocavalcante/oakberryio/internal/mmds"
	"github.com/iagocavalcante/oakberryio/internal/rootfs"
	"github.com/iagocavalcante/oakberryio/internal/secrets"
	"github.com/iagocavalcante/oakberryio/internal/store"
	"github.com/iagocavalcante/oakberryio/internal/tunnel"
	"github.com/iagocavalcante/oakberryio/internal/vm"
)

// Runtime performs the platform-specific halves of a deploy: turning an
// image reference into a bootable rootfs, and booting a microVM from a
// vm.Spec. See runtime_linux.go for the real implementation.
type Runtime interface {
	BuildRootfs(ctx context.Context, image string, out string) (*rootfs.ImageMeta, error)
	Start(ctx context.Context, spec vm.Spec, meta mmds.Guest) (Handle, error)
}

// Handle is a running machine, however Runtime started it.
type Handle interface {
	PID() int
	Stop(ctx context.Context) error
	// Wait blocks until the underlying process exits, however it exits (a
	// clean shutdown from inside the guest, a crash, or Stop's own kill).
	// The Deployer uses it to notice an unexpected exit and reconcile the
	// store, see watchMachine.
	Wait(ctx context.Context) error
}

// Checker is a single health check against a machine's service.
type Checker interface {
	Healthy(ctx context.Context, ip string, port int, path string) error
}

// vsockGuestCID is the guest Context Identifier oakd assigns every
// machine's vsock device. All machines use the same CID: each one is a
// separate Firecracker process with its own vsock namespace (the host UDS
// path, not the CID, is what makes one machine's vsock device distinct from
// another's), so there is no need to allocate a unique CID per machine.
const vsockGuestCID uint32 = 3

// vsockSocketPath returns the host-side Unix-domain socket path Firecracker
// exposes for machine id's vsock device, given oakd's socket directory. Used
// both when booting a machine (vm.Spec.VsockUDS) and by the ssh bridge
// (handleSSH) to dial it.
func vsockSocketPath(socketDir, id string) string {
	return filepath.Join(socketDir, id+"_vsock.sock")
}

// releaseCmd is the JSON encoding written to the releases.cmd column: the
// image's entrypoint and cmd kept as separate slices (rather than a single
// concatenated one) so an empty entrypoint round-trips distinctly from an
// empty cmd. Nothing else reads this column yet except Reconcile, via
// decodeReleaseCmd.
type releaseCmd struct {
	Entrypoint []string `json:"entrypoint"`
	Cmd        []string `json:"cmd"`
}

// Deployer runs the deploy flow: build rootfs, allocate resources, boot,
// health check, roll over, and re-render the tunnel config.
type Deployer struct {
	Store    *store.Store
	Runtime  Runtime
	Checker  Checker
	DataDir  string
	LogDir   string
	Identity *age.X25519Identity // nil if secrets aren't configured; buildGuestEnv skips them then

	// BaseCtx is the context passed to Runtime.Start (via
	// context.WithoutCancel), instead of the per-call ctx a caller like an
	// HTTP handler passes to Deploy. firecracker-go-sdk's VMCommandBuilder
	// runs the VMM under exec.CommandContext(ctx, ...): if that ctx is the
	// request's own context, it's cancelled the instant the HTTP handler
	// returns and kills the VMM immediately after boot. BaseCtx should live
	// for the daemon process's lifetime (e.g. the ctx cancelled by SIGTERM in
	// cmd/oakd/main.go) so machines outlive the request that started them.
	// Defaults to context.Background() when nil, so existing callers/tests
	// that never set it keep working.
	BaseCtx context.Context

	Kernel    string // defaults to /var/lib/oak/kernel/vmlinux, see kernel()
	SocketDir string // defaults to <DataDir>/run, see socketDir()

	Domain       string
	TunnelID     string
	TunnelConfig string // path cloudflared reads its config from
	TunnelCreds  string // cloudflared credentials-file path; defaults per credsFile()
	APIPort      int

	// HealthTimeout/HealthInterval override the health-check poll budget
	// (default 60s / 1s) -- present so tests can exercise a failing health
	// check without actually waiting a minute.
	HealthTimeout  time.Duration
	HealthInterval time.Duration

	// shuttingDown is set by StopAll when the daemon is being stopped. While
	// set, watchMachine leaves each machine's row 'running' instead of
	// deleting it on the exit StopAll triggers, so a graceful `systemctl
	// restart oakd` recovers exactly like a crash does: Reconcile restarts
	// every 'running' row on the next start.
	shuttingDown atomic.Bool

	mu         sync.Mutex
	handles    map[string]Handle
	appConfigs map[string]*appconfig.Config

	// tunnelMu serializes applyTunnel end to end (read running machines,
	// render, write the file, restart cloudflared). Deploy, Reconcile and
	// watchMachine (from its own goroutine, on any machine's exit) can all
	// call it concurrently; without this, two callers' writeAtomic calls
	// race on the same "<path>.tmp" and two "systemctl restart cloudflared"
	// runs race with each other. Deliberately a separate lock from mu:
	// applyTunnel calls appConfig, which takes mu itself, and holding one
	// lock while blocking to acquire the other would risk deadlock if any
	// future caller ever took them in the opposite order.
	tunnelMu sync.Mutex
}

// Deploy runs one deploy of image for the app described by cfg: it builds
// and boots a new machine, health-checks it, and on success stops the app's
// previous machines and re-renders the tunnel config. On any failure the new
// machine is torn down and its DB row removed (IPs are only ever held by
// live machine rows in this design) so nothing is left half-provisioned.
//
// progress, if non-nil, is called with human-readable lines as the deploy
// proceeds (e.g. for streaming to an HTTP client). It is a per-call
// parameter rather than a Deployer field so that concurrent Deploy calls for
// different apps never cross-write into each other's progress stream.
func (d *Deployer) Deploy(ctx context.Context, cfg *appconfig.Config, image string, progress func(string)) (string, error) {
	emit := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}

	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal config for %s: %w", cfg.App, err)
	}
	if err := d.Store.UpsertApp(cfg.App, string(configJSON)); err != nil {
		return "", fmt.Errorf("upsert app %s: %w", cfg.App, err)
	}
	d.cacheConfig(cfg.App, cfg)

	emit("building rootfs...\n")
	// ponytail: old rootfs images under <DataDir>/images/<app>/ from previous
	// releases are never pruned; add a retention sweep (keep last N) if disk
	// usage becomes a problem.
	rootfsPath := filepath.Join(d.DataDir, "images", cfg.App, fmt.Sprintf("%d.ext4", time.Now().UnixNano()))
	if err := os.MkdirAll(filepath.Dir(rootfsPath), 0755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(rootfsPath), err)
	}
	meta, err := d.Runtime.BuildRootfs(ctx, image, rootfsPath)
	if err != nil {
		return "", fmt.Errorf("build rootfs: %w", err)
	}

	cmdJSON, err := json.Marshal(releaseCmd{Entrypoint: meta.Entrypoint, Cmd: meta.Cmd})
	if err != nil {
		return "", fmt.Errorf("marshal release cmd: %w", err)
	}
	envJSON, err := json.Marshal(meta.Env)
	if err != nil {
		return "", fmt.Errorf("marshal release env: %w", err)
	}
	releaseID, err := d.Store.InsertRelease(cfg.App, image, rootfsPath, string(cmdJSON), string(envJSON), meta.WorkingDir)
	if err != nil {
		return "", fmt.Errorf("insert release: %w", err)
	}

	rel := store.Release{
		ID:      releaseID,
		App:     cfg.App,
		Image:   image,
		RootFS:  rootfsPath,
		Cmd:     string(cmdJSON),
		Env:     string(envJSON),
		Workdir: meta.WorkingDir,
	}
	return d.bootFromRelease(ctx, cfg, rel, progress)
}

// bootFromRelease is Deploy's "boot half": it builds a fresh VM spec/guest
// from cfg (VM size, env, services, volumes) and rel (rootfs, entrypoint,
// cmd, env, workdir -- decoded the same way reconcileOne decodes a stored
// release), boots it, health-gates the cutover, and on success stops the
// app's previous machines and re-renders the tunnel config. Deploy calls
// this right after inserting a brand new release; Restart and Scale call it
// with the app's existing latest release to reboot without a rebuild.
//
// On any failure the new machine is torn down and its DB row removed, same
// as Deploy's original inline failure-cleanup paths.
func (d *Deployer) bootFromRelease(ctx context.Context, cfg *appconfig.Config, rel store.Release, progress func(string)) (string, error) {
	emit := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}

	rc, err := decodeReleaseCmd(rel.Cmd)
	if err != nil {
		return "", err
	}
	imageEnv, err := decodeReleaseEnv(rel.Env)
	if err != nil {
		return "", err
	}

	volSpecs, mounts, err := d.resolveVolumes(cfg)
	if err != nil {
		return "", fmt.Errorf("resolve volumes: %w", err)
	}

	id, err := newMachineID()
	if err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	tap := "oak-" + id[:8]

	ip, err := d.Store.AllocAndInsertMachine(id, cfg.App, rel.ID, tap)
	if err != nil {
		return "", fmt.Errorf("alloc ip and insert machine %s: %w", id, err)
	}
	mac, err := vm.MACFromIP(ip)
	if err != nil {
		return "", fmt.Errorf("mac from ip %s: %w", ip, err)
	}

	envMap, err := d.buildGuestEnv(cfg, imageEnv)
	if err != nil {
		return "", fmt.Errorf("build guest env: %w", err)
	}

	logDir := filepath.Join(d.LogDir, cfg.App)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir log dir %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, id+".log")

	guest := mmds.Guest{
		MachineID:  id,
		App:        cfg.App,
		IP:         ip + "/16",
		Gateway:    "10.200.0.1",
		DNS:        "10.200.0.1",
		Env:        envMap,
		Entrypoint: rc.Entrypoint,
		Cmd:        rc.Cmd,
		WorkingDir: rel.Workdir,
		Mounts:     mounts,
	}
	spec := vm.Spec{
		ID:        id,
		Kernel:    d.kernel(),
		RootFS:    rel.RootFS,
		Volumes:   volSpecs,
		Tap:       tap,
		MAC:       mac,
		IP:        ip + "/16",
		Gateway:   "10.200.0.1",
		MemoryMB:  int64(cfg.VM.MemoryMB),
		CPUs:      int64(cfg.VM.CPUs),
		LogPath:   logPath,
		SocketDir: d.socketDir(),
		VsockUDS:  vsockSocketPath(d.socketDir(), id),
		GuestCID:  vsockGuestCID,
	}

	// A leftover socket file at this machine ID's path (e.g. from a previous
	// crash of oakd itself, or of Firecracker, before this ID's row was
	// cleaned up) makes firecracker-go-sdk's Config.Validate fail with "file
	// already exists" before it ever tries to boot.
	if err := os.Remove(filepath.Join(d.socketDir(), id+".sock")); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale socket for %s: %w", id, err)
	}
	if err := os.Remove(spec.VsockUDS); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale vsock socket for %s: %w", id, err)
	}

	emit("starting machine...\n")
	// Runtime.Start's context must outlive this call: firecracker-go-sdk
	// runs the VMM under exec.CommandContext, which kills the process the
	// instant its context is cancelled, and ctx here is the caller's
	// per-request context (cancelled once the deploy HTTP handler returns).
	// See BaseCtx's doc comment.
	handle, err := d.Runtime.Start(context.WithoutCancel(d.baseCtx()), spec, guest)
	if err != nil {
		_ = d.Store.SetMachineState(id, "failed", 0)
		_ = d.Store.DeleteMachine(id)
		return "", fmt.Errorf("start machine: %w", err)
	}
	d.setHandle(id, handle)

	if len(cfg.Services) > 0 {
		emit("waiting for health check...\n")
	}
	if err := d.awaitHealthy(ctx, cfg, ip); err != nil {
		_ = handle.Stop(ctx)
		d.removeHandle(id)
		_ = d.Store.SetMachineState(id, "failed", 0)
		_ = d.Store.DeleteMachine(id)
		return "", fmt.Errorf("%w\n--- last 50 lines of %s ---\n%s", err, logPath, readLastLines(logPath, 50))
	}

	if err := d.Store.SetMachineState(id, "running", handle.PID()); err != nil {
		return "", fmt.Errorf("mark machine %s running: %w", id, err)
	}
	go d.watchMachine(id, handle)

	if err := d.stopOldMachines(ctx, cfg.App, id); err != nil {
		return id, fmt.Errorf("stop previous machines for %s: %w", cfg.App, err)
	}

	if err := d.applyTunnel(ctx); err != nil {
		if errors.Is(err, tunnel.ErrRestart) {
			// The config is on disk; cloudflared just didn't reload it. The
			// deploy itself succeeded -- surface a warning rather than
			// failing a deploy whose machine is up and healthy.
			emit(fmt.Sprintf("tunnel restart failed: %v; deploy is live\n", err))
			return id, nil
		}
		return id, fmt.Errorf("apply tunnel config: %w", err)
	}

	return id, nil
}

// Restart reboots app from its latest release with no rebuild: it loads the
// app's stored config and boots a fresh machine from the newest release row
// via bootFromRelease, which health-gates the cutover exactly like Deploy.
// Errors if the app has no release yet.
func (d *Deployer) Restart(ctx context.Context, app string, progress func(string)) (string, error) {
	cfg, err := d.appConfig(app)
	if err != nil {
		return "", err
	}
	rel, err := d.Store.LatestRelease(app)
	if err != nil {
		return "", fmt.Errorf("restart %s: %w", app, err)
	}
	return d.bootFromRelease(ctx, cfg, rel, progress)
}

// Scale updates app's VM size (memory and/or CPU count) and reboots it from
// its latest release so the new machine picks up the change. A zero
// memoryMB or cpus leaves that field unchanged; callers (handleScale, the
// CLI) are responsible for rejecting a request where both are zero.
func (d *Deployer) Scale(ctx context.Context, app string, memoryMB, cpus int, progress func(string)) (string, error) {
	cfg, err := d.appConfig(app)
	if err != nil {
		return "", err
	}
	// Copy rather than mutate the (possibly cached) *appconfig.Config
	// appConfig returned: if UpsertApp below fails, the in-memory cache must
	// not have already been updated to a size that was never persisted.
	scaled := *cfg
	if memoryMB != 0 {
		scaled.VM.MemoryMB = memoryMB
	}
	if cpus != 0 {
		scaled.VM.CPUs = cpus
	}

	configJSON, err := json.Marshal(&scaled)
	if err != nil {
		return "", fmt.Errorf("marshal config for %s: %w", app, err)
	}
	if err := d.Store.UpsertApp(app, string(configJSON)); err != nil {
		return "", fmt.Errorf("upsert app %s: %w", app, err)
	}
	d.cacheConfig(app, &scaled)

	rel, err := d.Store.LatestRelease(app)
	if err != nil {
		return "", fmt.Errorf("scale %s: %w", app, err)
	}
	return d.bootFromRelease(ctx, &scaled, rel, progress)
}

// Destroy stops every machine for app, deletes all of its rows (machines,
// then releases/secrets/volumes, then the app itself), removes its on-disk
// artifacts, and re-renders the tunnel config so its route disappears.
// Machines are stopped and deleted first because both releases and machines
// carry a foreign key on apps.name; deleting the app row before that would
// fail (or, if it somehow succeeded, orphan those rows).
func (d *Deployer) Destroy(ctx context.Context, app string) error {
	machines, err := d.Store.MachinesForApp(app)
	if err != nil {
		return fmt.Errorf("list machines for %s: %w", app, err)
	}
	for _, m := range machines {
		if handle := d.getHandle(m.ID); handle != nil {
			_ = handle.Stop(ctx) // best-effort: still remove the DB row below either way
		}
		// ponytail: as in stopOldMachines, a machine with no live handle
		// (daemon restarted since it started, or a crash) leaves any
		// underlying Firecracker process as an orphan; out of scope for v1
		// per the design doc. The row is still deleted so the app delete
		// below never trips the machines->apps foreign key.
		if err := d.Store.DeleteMachine(m.ID); err != nil {
			return fmt.Errorf("delete machine %s: %w", m.ID, err)
		}
		// Deleting the row before removing the handle (same order as
		// stopOldMachines) means a concurrent watchMachine, once handle.Wait
		// returns, finds the row already gone via Store.Machine and no-ops
		// instead of racing this delete.
		d.removeHandle(m.ID)
	}

	if err := d.Store.DeleteReleasesForApp(app); err != nil {
		return err
	}
	if err := d.Store.DeleteSecretsForApp(app); err != nil {
		return err
	}
	if err := d.Store.DeleteVolumesForApp(app); err != nil {
		return err
	}
	if err := d.Store.DeleteApp(app); err != nil {
		return err
	}

	d.mu.Lock()
	delete(d.appConfigs, app)
	d.mu.Unlock()

	// Remove on-disk artifacts. Registry blobs (<registry>/<app>:*) are left
	// in place.
	// ponytail: no registry GC here; add a sweep of the registry's blob
	// store keyed by app name if disk usage from destroyed apps' images
	// becomes a problem.
	if err := os.RemoveAll(filepath.Join(d.DataDir, "images", app)); err != nil {
		return fmt.Errorf("remove images for %s: %w", app, err)
	}
	if err := os.RemoveAll(filepath.Join(d.LogDir, app)); err != nil {
		return fmt.Errorf("remove logs for %s: %w", app, err)
	}
	volumeImgs, err := filepath.Glob(filepath.Join(d.DataDir, "volumes", app+"-*.img"))
	if err != nil {
		return fmt.Errorf("glob volumes for %s: %w", app, err)
	}
	for _, p := range volumeImgs {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("remove volume file %s: %w", p, err)
		}
	}

	// Same ErrRestart carve-out as Deploy: the app and its rows/files are
	// already fully gone at this point, so a cloudflared reload hiccup (the
	// new config, minus this app's route, is already on disk) shouldn't be
	// reported as a failed destroy. Destroy has no progress channel to warn
	// on, so just log, matching watchMachine's own best-effort re-apply.
	if err := d.applyTunnel(ctx); err != nil {
		if errors.Is(err, tunnel.ErrRestart) {
			log.Printf("oakd: tunnel restart failed after destroying %s: %v", app, err)
			return nil
		}
		return fmt.Errorf("apply tunnel config: %w", err)
	}
	return nil
}

// watchMachine blocks until handle's underlying process exits, then -- only
// if the store still shows this exact machine (by ID and PID) as "running",
// meaning nothing else has already superseded it (a later deploy's
// stopOldMachines, or Reconcile after a daemon restart) -- marks it stopped,
// drops the in-memory handle, and best-effort re-renders the tunnel config
// so the dead machine's route disappears. Runs for the lifetime of the
// daemon process, hence BaseCtx rather than any per-call ctx.
func (d *Deployer) watchMachine(id string, handle Handle) {
	waitErr := handle.Wait(context.WithoutCancel(d.baseCtx()))

	// If the daemon is shutting down, this exit was triggered by StopAll.
	// Leave the row 'running' (and the tunnel untouched) so Reconcile brings
	// the machine back on the next start; deleting it here is what made a
	// graceful `systemctl restart oakd` lose every machine.
	if d.shuttingDown.Load() {
		return
	}

	m, err := d.Store.Machine(id)
	if err != nil {
		// Row is gone entirely: another path (stopOldMachines, a failed
		// deploy's cleanup) already deleted it. Nothing to reconcile.
		return
	}
	if m.State != "running" || int(m.PID) != handle.PID() {
		// Superseded by a later deploy/reconcile before this exit was
		// observed; that path owns the row now.
		return
	}

	if waitErr != nil {
		log.Printf("oakd: machine %s exited with error: %v", id, waitErr)
	}
	if err := d.Store.SetMachineState(id, "stopped", 0); err != nil {
		log.Printf("oakd: mark machine %s stopped after exit: %v", id, err)
	}
	d.removeHandle(id)
	// Delete the row outright rather than leaving it "stopped": this design
	// only ever holds an IP against a live "running" row (see
	// stopOldMachines/Deploy's failure-cleanup paths, which do the same
	// SetMachineState-then-DeleteMachine sequence), so leaving a "stopped"
	// row behind would leak both the row and its IP on every unexpected
	// exit. SetMachineState above still runs first so the transition is
	// visible to anything racing to read the row in between.
	if err := d.Store.DeleteMachine(id); err != nil {
		log.Printf("oakd: delete machine %s after exit: %v", id, err)
	}
	if err := d.applyTunnel(context.WithoutCancel(d.baseCtx())); err != nil {
		log.Printf("oakd: re-apply tunnel after machine %s exited: %v", id, err)
	}
}

// awaitHealthy polls Checker.Healthy for up to the configured budget
// (default 60s, every 1s). An app with no services defined has nothing to
// check and is considered healthy immediately.
func (d *Deployer) awaitHealthy(ctx context.Context, cfg *appconfig.Config, ip string) error {
	if len(cfg.Services) == 0 {
		return nil
	}
	svc := cfg.Services[0]

	timeout := d.HealthTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	interval := d.HealthInterval
	if interval <= 0 {
		interval = time.Second
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		lastErr = d.Checker.Healthy(ctx, ip, svc.InternalPort, svc.Check.Path)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check never passed: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// stopOldMachines stops and deletes every other running machine for app.
// IPs in this design are only ever held by live machine rows, so a stopped
// machine is deleted outright rather than kept around in a "stopped" state;
// the releases table already preserves history.
func (d *Deployer) stopOldMachines(ctx context.Context, app, newID string) error {
	machines, err := d.Store.MachinesForApp(app)
	if err != nil {
		return fmt.Errorf("list machines for %s: %w", app, err)
	}
	for _, m := range machines {
		if m.ID == newID || m.State != "running" {
			continue
		}
		if handle := d.getHandle(m.ID); handle != nil {
			_ = handle.Stop(ctx) // best-effort: still remove the DB row below either way
		}
		// ponytail: if there's no handle (daemon restarted since this
		// machine was started and Reconcile hasn't run yet, or a crash),
		// there's nothing to stop -- the underlying Firecracker process, if
		// any, is an orphan. Out of scope for v1 per the design doc ("crash
		// of oakd kills VMs"); the DB row is still deleted so the IP frees.
		if err := d.Store.DeleteMachine(m.ID); err != nil {
			return fmt.Errorf("delete old machine %s: %w", m.ID, err)
		}
		d.removeHandle(m.ID)
	}
	return nil
}

// resolveVolumes matches cfg's [[mounts]] entries against app's created
// volumes, in mount order, assigning /dev/vdb, /dev/vdc, ... in that same
// order (the root disk is implicitly /dev/vda via vm.Spec.RootFS).
func (d *Deployer) resolveVolumes(cfg *appconfig.Config) ([]string, []mmds.Mount, error) {
	if len(cfg.Mounts) == 0 {
		return nil, nil, nil
	}
	available, err := d.Store.Volumes(cfg.App)
	if err != nil {
		return nil, nil, fmt.Errorf("list volumes for %s: %w", cfg.App, err)
	}
	byName := make(map[string]store.Volume, len(available))
	for _, v := range available {
		byName[v.Name] = v
	}

	volSpecs := make([]string, 0, len(cfg.Mounts))
	mounts := make([]mmds.Mount, 0, len(cfg.Mounts))
	for i, m := range cfg.Mounts {
		v, ok := byName[m.Volume]
		if !ok {
			return nil, nil, fmt.Errorf("mount references unknown volume %q for app %s (create it first)", m.Volume, cfg.App)
		}
		volSpecs = append(volSpecs, v.Path)
		mounts = append(mounts, mmds.Mount{
			Device: fmt.Sprintf("/dev/vd%c", 'b'+i),
			Dest:   m.Destination,
		})
	}
	return volSpecs, mounts, nil
}

// buildGuestEnv resolves the guest's final environment: image env as the
// base, app config [env] overriding it, decrypted secrets overriding that,
// and PORT defaulted to the first service's internal_port if the app
// declares services and PORT isn't already set by any of the above layers.
func (d *Deployer) buildGuestEnv(cfg *appconfig.Config, imageEnv []string) (map[string]string, error) {
	override := make(map[string]string, len(cfg.Env))
	for k, v := range cfg.Env {
		override[k] = v
	}

	if d.Identity != nil && d.Store != nil {
		encrypted, err := d.Store.Secrets(cfg.App)
		if err != nil {
			return nil, fmt.Errorf("load secrets for %s: %w", cfg.App, err)
		}
		identity := d.Identity.String()
		for k, ct := range encrypted {
			pt, err := secrets.Decrypt(identity, ct)
			if err != nil {
				return nil, fmt.Errorf("decrypt secret %s/%s: %w", cfg.App, k, err)
			}
			override[k] = string(pt)
		}
	}

	if len(cfg.Services) > 0 {
		_, inOverride := override["PORT"]
		if !inOverride && !envSliceHasKey(imageEnv, "PORT") {
			override["PORT"] = strconv.Itoa(cfg.Services[0].InternalPort)
		}
	}

	if _, ok := override["PATH"]; !ok && !envSliceHasKey(imageEnv, "PATH") {
		override["PATH"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	return envSliceToMap(mmds.MergeEnv(imageEnv, override)), nil
}

func envSliceHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// applyTunnel re-renders and applies the cloudflared config from every
// currently running machine across all apps, plus the fixed route to oak's
// own API. A no-op when the tunnel isn't configured (e.g. TunnelID unset in
// tests or before the operator has run `cloudflared tunnel create`).
func (d *Deployer) applyTunnel(ctx context.Context) error {
	if d.TunnelID == "" || d.TunnelConfig == "" {
		return nil
	}
	d.tunnelMu.Lock()
	defer d.tunnelMu.Unlock()

	machines, err := d.Store.RunningMachines()
	if err != nil {
		return fmt.Errorf("list running machines: %w", err)
	}

	routes := make([]tunnel.Route, 0, len(machines)+1)
	for _, m := range machines {
		cfg, err := d.appConfig(m.App)
		if err != nil {
			return fmt.Errorf("app config for %s: %w", m.App, err)
		}
		if len(cfg.Services) == 0 {
			continue // nothing to route to without a declared service port
		}
		service := fmt.Sprintf("http://%s:%d", m.IP, cfg.Services[0].InternalPort)
		routes = append(routes, tunnel.Route{
			Hostname: fmt.Sprintf("%s.%s", m.App, d.Domain),
			Service:  service,
		})
		for _, domain := range cfg.Domains {
			routes = append(routes, tunnel.Route{
				Hostname: domain,
				Service:  service,
			})
		}
	}
	routes = append(routes, tunnel.Route{
		Hostname: fmt.Sprintf("oak.%s", d.Domain),
		Service:  fmt.Sprintf("http://127.0.0.1:%d", d.APIPort),
	})

	data := tunnel.Render(d.TunnelID, d.credsFile(), routes)
	return tunnel.Apply(d.TunnelConfig, data)
}

// appConfig returns app's parsed config, loading and caching it from the
// store on first use. The cache also serves Reconcile, which runs before
// any Deploy call has populated it for a given app.
func (d *Deployer) appConfig(app string) (*appconfig.Config, error) {
	d.mu.Lock()
	if cfg, ok := d.appConfigs[app]; ok {
		d.mu.Unlock()
		return cfg, nil
	}
	d.mu.Unlock()

	raw, err := d.Store.AppConfig(app)
	if err != nil {
		return nil, fmt.Errorf("load app config %s: %w", app, err)
	}
	var cfg appconfig.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode app config %s: %w", app, err)
	}
	d.cacheConfig(app, &cfg)
	return &cfg, nil
}

func (d *Deployer) cacheConfig(app string, cfg *appconfig.Config) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.appConfigs == nil {
		d.appConfigs = make(map[string]*appconfig.Config)
	}
	d.appConfigs[app] = cfg
}

func (d *Deployer) setHandle(id string, h Handle) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.handles == nil {
		d.handles = make(map[string]Handle)
	}
	d.handles[id] = h
}

func (d *Deployer) getHandle(id string) Handle {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.handles[id]
}

func (d *Deployer) removeHandle(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.handles, id)
}

// StopAll gracefully stops every machine this process currently holds a
// live handle for, returning any per-machine stop errors keyed by machine
// ID. Used on daemon shutdown.
func (d *Deployer) StopAll(ctx context.Context) map[string]error {
	// Mark shutdown before stopping anything so every watchMachine goroutine
	// sees it when the Stop below makes its handle.Wait return, and leaves the
	// row 'running' for Reconcile instead of deleting it.
	d.shuttingDown.Store(true)

	d.mu.Lock()
	handles := make(map[string]Handle, len(d.handles))
	for id, h := range d.handles {
		handles[id] = h
	}
	d.mu.Unlock()

	errs := make(map[string]error)
	for id, h := range handles {
		if err := h.Stop(ctx); err != nil {
			errs[id] = err
		}
		d.removeHandle(id)
	}
	return errs
}

func (d *Deployer) kernel() string {
	if d.Kernel != "" {
		return d.Kernel
	}
	return "/var/lib/oak/kernel/vmlinux"
}

func (d *Deployer) socketDir() string {
	if d.SocketDir != "" {
		return d.SocketDir
	}
	return filepath.Join(d.DataDir, "run")
}

// baseCtx returns BaseCtx, defaulting to context.Background() so Deployers
// built directly (tests, or any future caller that never sets BaseCtx) don't
// hand context.WithoutCancel a nil parent, which panics.
func (d *Deployer) baseCtx() context.Context {
	if d.BaseCtx != nil {
		return d.BaseCtx
	}
	return context.Background()
}

func (d *Deployer) credsFile() string {
	if d.TunnelCreds != "" {
		return d.TunnelCreds
	}
	return fmt.Sprintf("/root/.cloudflared/%s.json", d.TunnelID)
}

// newMachineID returns a random 12-hex-char machine ID.
func newMachineID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// readLastLines returns the last n lines of the file at path, or a
// placeholder describing why it couldn't be read.
func readLastLines(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(could not read log %s: %v)", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// decodeReleaseCmd parses the JSON stored in a release's cmd column.
func decodeReleaseCmd(cmdJSON string) (releaseCmd, error) {
	var rc releaseCmd
	if err := json.Unmarshal([]byte(cmdJSON), &rc); err != nil {
		return releaseCmd{}, fmt.Errorf("decode release cmd: %w", err)
	}
	return rc, nil
}

// decodeReleaseEnv parses the JSON stored in a release's env column (the
// image's raw baked-in env, pre-merge with app config or secrets).
func decodeReleaseEnv(envJSON string) ([]string, error) {
	var env []string
	if err := json.Unmarshal([]byte(envJSON), &env); err != nil {
		return nil, fmt.Errorf("decode release env: %w", err)
	}
	return env, nil
}

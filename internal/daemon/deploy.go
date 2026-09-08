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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
}

// Checker is a single health check against a machine's service.
type Checker interface {
	Healthy(ctx context.Context, ip string, port int, path string) error
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

	mu         sync.Mutex
	handles    map[string]Handle
	appConfigs map[string]*appconfig.Config
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

	volSpecs, mounts, err := d.resolveVolumes(cfg)
	if err != nil {
		return "", fmt.Errorf("resolve volumes: %w", err)
	}

	ip, err := d.Store.AllocIP()
	if err != nil {
		return "", fmt.Errorf("alloc ip: %w", err)
	}
	mac, err := vm.MACFromIP(ip)
	if err != nil {
		return "", fmt.Errorf("mac from ip %s: %w", ip, err)
	}
	id, err := newMachineID()
	if err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	tap := "oak-" + id[:8]

	if err := d.Store.InsertMachine(id, cfg.App, releaseID, ip, tap); err != nil {
		return "", fmt.Errorf("insert machine %s: %w", id, err)
	}

	envMap, err := d.buildGuestEnv(cfg, meta.Env)
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
		Entrypoint: meta.Entrypoint,
		Cmd:        meta.Cmd,
		WorkingDir: meta.WorkingDir,
		Mounts:     mounts,
	}
	spec := vm.Spec{
		ID:        id,
		Kernel:    d.kernel(),
		RootFS:    rootfsPath,
		Volumes:   volSpecs,
		Tap:       tap,
		MAC:       mac,
		MemoryMB:  int64(cfg.VM.MemoryMB),
		CPUs:      int64(cfg.VM.CPUs),
		LogPath:   logPath,
		SocketDir: d.socketDir(),
	}

	emit("starting machine...\n")
	handle, err := d.Runtime.Start(ctx, spec, guest)
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

	if err := d.stopOldMachines(ctx, cfg.App, id); err != nil {
		return id, fmt.Errorf("stop previous machines for %s: %w", cfg.App, err)
	}

	if err := d.applyTunnel(ctx); err != nil {
		return id, fmt.Errorf("apply tunnel config: %w", err)
	}

	return id, nil
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
		routes = append(routes, tunnel.Route{
			Hostname: fmt.Sprintf("%s.%s", m.App, d.Domain),
			Service:  fmt.Sprintf("http://%s:%d", m.IP, cfg.Services[0].InternalPort),
		})
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

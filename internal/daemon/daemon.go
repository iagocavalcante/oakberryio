package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"filippo.io/age"

	"github.com/iagocavalcante/oakberryio/internal/appconfig"
	"github.com/iagocavalcante/oakberryio/internal/metrics"
	"github.com/iagocavalcante/oakberryio/internal/mmds"
	"github.com/iagocavalcante/oakberryio/internal/secrets"
	"github.com/iagocavalcante/oakberryio/internal/store"
	"github.com/iagocavalcante/oakberryio/internal/tunnel"
	"github.com/iagocavalcante/oakberryio/internal/vm"
)

// metricsSampleInterval is how often the daemon recomputes the metrics
// snapshot the control panel reads; see docs/plans/
// 2026-09-15-control-panel-design.md. Sampling in the background (not per
// request) keeps GET /metrics cheap and lets CPU% be a real delta between
// samples, not a single instantaneous reading.
const metricsSampleInterval = 2 * time.Second

// metricsHolder stores the latest metrics.Snapshot behind a mutex. It's its
// own tiny type, rather than a field directly on Daemon or API, so the
// sampling goroutine (writer, in Run) and handleMetrics (reader, in api.go)
// can share one lock without either struct needing to reach into the
// other.
type metricsHolder struct {
	mu   sync.Mutex
	snap metrics.Snapshot
}

func (h *metricsHolder) set(s metrics.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snap = s
}

func (h *metricsHolder) get() metrics.Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

// StaticRoute is one operator-configured cloudflared ingress rule that
// isn't tied to any running app machine -- e.g. exposing a host-level
// service like the control panel. See docs/host.md ("Static routes").
type StaticRoute struct {
	Hostname string `toml:"hostname"`
	Service  string `toml:"service"`
}

// Config is oakd's configuration, loaded from /etc/oak/oakd.toml.
type Config struct {
	DataDir      string `toml:"data_dir"`
	Registry     string `toml:"registry"`
	Domain       string `toml:"domain"`
	TunnelID     string `toml:"tunnel_id"`
	TunnelConfig string `toml:"tunnel_config"`
	// TunnelCreds is the cloudflared credentials-file path. Not in the
	// original oakd.toml sample; added because tunnel.Render needs one and
	// there's no other sensible source for it. Defaults to
	// /root/.cloudflared/<tunnel_id>.json (where `cloudflared tunnel
	// create` writes it as root, per docs/host.md) when left empty.
	TunnelCreds string `toml:"tunnel_creds"`
	// StaticRoutes are extra cloudflared ingress rules applied verbatim on
	// top of the per-app machine routes -- for a host service that isn't
	// backed by any oakd-managed machine (e.g. the control panel). See
	// docs/host.md.
	StaticRoutes []StaticRoute `toml:"static_routes"`
	KeyFile      string        `toml:"key_file"`
	Socket       string        `toml:"socket"`

	// LogDir and APIToken are new fields for Task 8; see docs/host.md.
	LogDir   string `toml:"log_dir"`
	APIToken string `toml:"api_token"`
	// APIPort is not in the original sample either, but the TCP listener
	// needs a port from somewhere; defaults to 7000 per the plan's text.
	APIPort int `toml:"api_port"`
}

func (c *Config) applyDefaults() {
	if c.LogDir == "" {
		c.LogDir = "/var/log/oak"
	}
	if c.APIPort == 0 {
		c.APIPort = 7000
	}
	if c.Socket == "" {
		c.Socket = "/run/oak.sock"
	}
	if c.Registry == "" {
		c.Registry = "localhost:5000"
	}
}

// Daemon wires together the store, deploy orchestrator, DNS resolver and
// HTTP API for one oakd process.
type Daemon struct {
	cfg      Config
	Store    *store.Store
	Deployer *Deployer
	API      *API

	// sampler computes host/VM resource-usage snapshots; see Run's metrics
	// goroutine. metricsHolder is the published result API.handleMetrics
	// reads.
	sampler       *metrics.Sampler
	metricsHolder *metricsHolder

	// bridgeMu serializes ensureBridge end to end (netlink bridge setup plus
	// starting a tenant subnet's first DNS listener) across concurrent
	// Deploy/Reconcile calls that might land on the same, or a brand new,
	// tenant subnet at the same time.
	bridgeMu sync.Mutex
	// dnsListening tracks which tenant subnet indexes already have a DNS
	// listener running, so ensureBridge starts each one exactly once (see
	// its doc comment: every bridge needs its own listener bound to its own
	// gateway IP).
	dnsListening map[int]bool
}

// New opens the store, loads the age identity (if KeyFile is set), and
// wires the daemon's components. It does not start listening or reconcile
// machines; call Reconcile then Run for that.
//
// ctx becomes the Deployer's BaseCtx: the context every microVM is started
// under (via context.WithoutCancel), independent of whatever per-request
// context triggers a given deploy. Pass a context tied to the daemon
// process's own lifetime (e.g. the one signal.NotifyContext returns in
// cmd/oakd/main.go), not a per-request one, or every machine will die the
// instant the request that started it completes.
func New(ctx context.Context, cfg Config) (*Daemon, error) {
	cfg.applyDefaults()

	st, err := store.Open(filepath.Join(cfg.DataDir, "oak.db"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	var identity *age.X25519Identity
	if cfg.KeyFile != "" {
		identity, err = secrets.Load(cfg.KeyFile)
		if err != nil {
			_ = st.Close()
			return nil, fmt.Errorf("load age identity from %s: %w", cfg.KeyFile, err)
		}
	}

	var staticRoutes []tunnel.Route
	for _, r := range cfg.StaticRoutes {
		if !appconfig.ValidDomain(r.Hostname) {
			log.Printf("oakd: static route %q: invalid hostname, skipping", r.Hostname)
			continue
		}
		staticRoutes = append(staticRoutes, tunnel.Route{Hostname: r.Hostname, Service: r.Service})
	}

	deployer := &Deployer{
		Store:        st,
		Runtime:      FirecrackerRuntime{},
		Checker:      HTTPChecker{},
		DataDir:      cfg.DataDir,
		LogDir:       cfg.LogDir,
		Identity:     identity,
		Registry:     cfg.Registry,
		BaseCtx:      ctx,
		Domain:       cfg.Domain,
		TunnelID:     cfg.TunnelID,
		TunnelConfig: cfg.TunnelConfig,
		TunnelCreds:  cfg.TunnelCreds,
		APIPort:      cfg.APIPort,
		StaticRoutes: staticRoutes,
	}

	if err := os.MkdirAll(deployer.socketDir(), 0755); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("mkdir socket dir %s: %w", deployer.socketDir(), err)
	}

	mh := &metricsHolder{}
	api := &API{Deployer: deployer, Store: st, Token: cfg.APIToken, Metrics: mh}

	d := &Daemon{
		cfg:           cfg,
		Store:         st,
		Deployer:      deployer,
		API:           api,
		sampler:       metrics.NewSampler(cfg.DataDir),
		metricsHolder: mh,
	}
	// deployer.EnsureBridge closes over d itself (built above) rather than
	// being inlined into the Deployer literal, since ensureBridge is a
	// Daemon method: bridge lifecycle and the DNS listeners it starts both
	// need Daemon-level state (bridgeMu/dnsListening, the shared resolver
	// over d.Store) that Deployer has no reason to own.
	deployer.EnsureBridge = d.ensureBridge

	return d, nil
}

// Reconcile re-starts every machine the store thinks is running. A daemon
// restart kills the Firecracker child processes that back them (see the
// design doc: "Crash of oakd kills VMs"), so on start every 'running' row
// needs a fresh Handle. A per-machine Start failure marks that machine
// failed and reconcile continues with the rest, then applies the tunnel
// config from whatever ended up running.
func (d *Daemon) Reconcile(ctx context.Context) error {
	machines, err := d.Store.RunningMachines()
	if err != nil {
		return fmt.Errorf("reconcile: list running machines: %w", err)
	}

	for _, m := range machines {
		if err := d.reconcileOne(ctx, m); err != nil {
			_ = d.Store.SetMachineState(m.ID, "failed", 0)
		}
	}

	return d.Deployer.applyTunnel(ctx)
}

func (d *Daemon) reconcileOne(ctx context.Context, m store.Machine) error {
	cfg, err := d.Deployer.appConfig(m.App)
	if err != nil {
		return fmt.Errorf("app config: %w", err)
	}

	rel, err := d.Store.ReleaseByID(m.ReleaseID)
	if err != nil {
		return fmt.Errorf("release %d: %w", m.ReleaseID, err)
	}
	rc, err := decodeReleaseCmd(rel.Cmd)
	if err != nil {
		return err
	}
	imageEnv, err := decodeReleaseEnv(rel.Env)
	if err != nil {
		return err
	}

	volSpecs, mounts, err := d.Deployer.resolveVolumes(cfg)
	if err != nil {
		return fmt.Errorf("resolve volumes: %w", err)
	}
	envMap, err := d.Deployer.buildGuestEnv(cfg, imageEnv)
	if err != nil {
		return fmt.Errorf("build guest env: %w", err)
	}
	mac, err := vm.MACFromIP(m.IP)
	if err != nil {
		return fmt.Errorf("mac from ip %s: %w", m.IP, err)
	}

	idx, err := d.Deployer.subnetForApp(m.App)
	if err != nil {
		return fmt.Errorf("subnet for machine %s: %w", m.ID, err)
	}
	bridge, gateway, err := d.Deployer.EnsureBridge(idx)
	if err != nil {
		return fmt.Errorf("ensure bridge for machine %s: %w", m.ID, err)
	}

	logDir := filepath.Join(d.Deployer.LogDir, m.App)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("mkdir log dir %s: %w", logDir, err)
	}

	spec := vm.Spec{
		ID:        m.ID,
		Kernel:    d.Deployer.kernel(),
		RootFS:    rel.RootFS,
		Volumes:   volSpecs,
		Tap:       m.Tap,
		Bridge:    bridge,
		MAC:       mac,
		IP:        m.IP + "/24",
		Gateway:   gateway,
		MemoryMB:  int64(cfg.VM.MemoryMB),
		CPUs:      int64(cfg.VM.CPUs),
		LogPath:   filepath.Join(logDir, m.ID+".log"),
		SocketDir: d.Deployer.socketDir(),
		VsockUDS:  vsockSocketPath(d.Deployer.socketDir(), m.ID),
		GuestCID:  vsockGuestCID,
	}
	guest := mmds.Guest{
		MachineID:  m.ID,
		App:        m.App,
		IP:         m.IP + "/24",
		Gateway:    gateway,
		DNS:        gateway,
		Env:        envMap,
		Entrypoint: rc.Entrypoint,
		Cmd:        rc.Cmd,
		WorkingDir: rel.Workdir,
		Mounts:     mounts,
	}

	// See Deploy's identical guard: a leftover socket file from before this
	// machine's previous run (crashed daemon, crashed Firecracker) fails
	// Config.Validate before Start ever gets to boot anything.
	if err := os.Remove(filepath.Join(d.Deployer.socketDir(), m.ID+".sock")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket for %s: %w", m.ID, err)
	}
	if err := os.Remove(spec.VsockUDS); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale vsock socket for %s: %w", m.ID, err)
	}

	// Same BaseCtx reasoning as Deploy: this must outlive Reconcile's own
	// ctx (bounded by the daemon startup sequence), or the VMM dies the
	// moment Reconcile returns.
	handle, err := d.Deployer.Runtime.Start(context.WithoutCancel(d.Deployer.baseCtx()), spec, guest)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	d.Deployer.setHandle(m.ID, handle)

	if err := d.Store.SetMachineState(m.ID, "running", handle.PID()); err != nil {
		return fmt.Errorf("set state: %w", err)
	}
	go d.Deployer.watchMachine(m.ID, handle)
	return nil
}

// Run ensures the admin tenant subnet's bridge/DNS listener (idx 0, so a
// listener always exists on 10.200.0.1:53 even before any machine has ever
// been deployed, matching the pre-Phase-D behavior of an always-on static
// listener there) and starts both API listeners (unix socket, trusted; TCP
// on 127.0.0.1:<api_port>, bearer-token guarded), blocking until ctx is
// cancelled or one of them fails. The TCP listener is skipped entirely when
// no APIToken is configured: AuthMiddleware would otherwise compare every
// request's bearer token against an empty string, so an unset token doesn't
// mean "no auth" -- it silently locks the API to a token nobody has. That's
// a worse failure mode than just not exposing the remote listener until an
// operator sets one.
func (d *Daemon) Run(ctx context.Context) error {
	errCh := make(chan error, 3)

	if _, _, err := d.ensureBridge(0); err != nil {
		return fmt.Errorf("ensure admin bridge: %w", err)
	}

	go d.sampleMetrics(ctx)

	mux := d.API.Mux()

	unixLn, err := listenUnix(d.cfg.Socket)
	if err != nil {
		return fmt.Errorf("unix listen: %w", err)
	}
	go func() {
		if err := http.Serve(unixLn, mux); err != nil && !errors.Is(err, net.ErrClosed) {
			errCh <- fmt.Errorf("unix api: %w", err)
		}
	}()

	var tcpLn net.Listener
	if d.cfg.APIToken == "" {
		log.Printf("oakd: api_token not set; serving only the unix socket, not 127.0.0.1:%d", d.cfg.APIPort)
	} else {
		tcpAddr := fmt.Sprintf("127.0.0.1:%d", d.cfg.APIPort)
		tcpLn, err = net.Listen("tcp", tcpAddr)
		if err != nil {
			_ = unixLn.Close()
			return fmt.Errorf("tcp listen %s: %w", tcpAddr, err)
		}
		go func() {
			if err := http.Serve(tcpLn, d.API.AuthMiddleware(mux)); err != nil && !errors.Is(err, net.ErrClosed) {
				errCh <- fmt.Errorf("tcp api: %w", err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		_ = unixLn.Close()
		if tcpLn != nil {
			_ = tcpLn.Close()
		}
		return nil
	case err := <-errCh:
		_ = unixLn.Close()
		if tcpLn != nil {
			_ = tcpLn.Close()
		}
		return err
	}
}

// sampleMetrics recomputes the metrics snapshot every metricsSampleInterval
// until ctx is done, publishing each result to d.metricsHolder for
// handleMetrics to read. A sampling failure (e.g. a transient /proc read
// error) is logged and skipped rather than fatal -- the control panel is a
// read-only convenience, not something worth taking the daemon down over.
func (d *Daemon) sampleMetrics(ctx context.Context) {
	ticker := time.NewTicker(metricsSampleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			machines, err := d.Store.RunningMachines()
			if err != nil {
				log.Printf("oakd: metrics: list running machines: %v", err)
				continue
			}
			pids := make([]metrics.MachinePID, len(machines))
			for i, m := range machines {
				pids[i] = metrics.MachinePID{ID: m.ID, PID: int(m.PID)}
			}
			snap, err := d.sampler.Sample(pids)
			if err != nil {
				log.Printf("oakd: metrics: sample: %v", err)
				continue
			}
			d.metricsHolder.set(snap)
		}
	}
}

func listenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen unix %s: %w", path, err)
	}
	return ln, nil
}

// StopAll gracefully stops every machine this process currently holds a
// live handle for. Used on daemon shutdown.
func (d *Daemon) StopAll(ctx context.Context) map[string]error {
	return d.Deployer.StopAll(ctx)
}

// Close closes the underlying store.
func (d *Daemon) Close() error {
	return d.Store.Close()
}

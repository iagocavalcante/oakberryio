package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// --- fakes ---------------------------------------------------------------

type fakeHandle struct {
	pid     int
	stopped bool

	// waitCh, when set, is what Wait blocks on; closing it (or sending an
	// error) makes Wait return that error (nil for a closed channel). Left
	// nil, Wait blocks until ctx is cancelled -- a nil channel's receive
	// case in the select below never fires, so this mirrors a real
	// still-running process's Wait: it doesn't return on its own just
	// because nobody's watching. Deploy's tests use context.Background() as
	// BaseCtx, so a Deployer's watchMachine goroutine for a handle that
	// never has its waitCh touched simply leaks for the life of the test
	// process rather than firing a spurious "stopped" transition that would
	// race e.g. TestDeployFailingHealthCheckLeavesOldRunningAndDeletesNew's
	// assertion that the first deploy's machine is still "running".
	waitCh chan error
}

func (h *fakeHandle) PID() int                       { return h.pid }
func (h *fakeHandle) Stop(ctx context.Context) error { h.stopped = true; return nil }

func (h *fakeHandle) Wait(ctx context.Context) error {
	select {
	case err := <-h.waitCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type fakeRuntime struct {
	mu       sync.Mutex
	nextPID  int
	starts   []vm.Spec
	startErr error
	env      []string

	// waitCh, when set, is handed to every fakeHandle Start creates, so a
	// test can control when that handle's Wait returns (see fakeHandle.Wait).
	waitCh chan error
	// startCtx captures the ctx passed to the most recent Start call, so a
	// test can assert on its lifetime independent of the ctx passed to
	// Deploy (see TestDeployStartCtxSurvivesRequestCancellation).
	startCtx context.Context

	// onStart, if set, runs synchronously inside Start with the spec and
	// guest just passed to it, and its return value -- if non-nil -- is
	// used as this call's handle's waitCh instead of the shared r.waitCh.
	// This lets a test single out one particular Start call (e.g. a
	// release-command run, identifiable by its shell entrypoint) to behave
	// differently from the others: writing a synthetic "oak-init:
	// child-exit status=<N>" line to spec.LogPath, the way oak-init's real
	// marker would appear (see lastChildExitStatus), and returning an
	// already-resolved channel so that machine's Wait returns immediately
	// while every other call keeps blocking on r.waitCh like before.
	onStart func(spec vm.Spec, guest mmds.Guest) chan error
}

func (r *fakeRuntime) BuildRootfs(ctx context.Context, image, out string) (*rootfs.ImageMeta, error) {
	env := r.env
	if env == nil {
		env = []string{"BASE_IMAGE_VAR=1"}
	}
	return &rootfs.ImageMeta{Entrypoint: []string{"/bin/app"}, Env: env, WorkingDir: "/"}, nil
}

func (r *fakeRuntime) Start(ctx context.Context, spec vm.Spec, meta mmds.Guest) (Handle, error) {
	if r.startErr != nil {
		return nil, r.startErr
	}
	waitCh := r.waitCh
	if r.onStart != nil {
		if ch := r.onStart(spec, meta); ch != nil {
			waitCh = ch
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPID++
	r.starts = append(r.starts, spec)
	r.startCtx = ctx
	return &fakeHandle{pid: r.nextPID, waitCh: waitCh}, nil
}

// writeChildExitMarker writes a log file containing oak-init's real
// child-exit marker line, simulating what the guest's serial console would
// contain after oak-init runs the release command and powers off -- so
// lastChildExitStatus (production code) can read it back exactly as it
// would a real machine's log.
func writeChildExitMarker(t *testing.T, path string, status int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	line := fmt.Sprintf("some app output\noak-init: child-exit status=%d\n", status)
	if err := os.WriteFile(path, []byte(line), 0644); err != nil {
		t.Fatalf("write log %s: %v", path, err)
	}
}

// resolvedChan returns a channel that already holds err, for onStart hooks
// that want a handle's Wait to return immediately rather than block on the
// shared fakeRuntime.waitCh.
func resolvedChan(err error) chan error {
	ch := make(chan error, 1)
	ch <- err
	return ch
}

// isReleaseCommandGuest reports whether guest's argv is a release-command
// invocation, i.e. runReleaseCommand's shell override ([]string{"/bin/sh",
// "-lc", cmd}), as opposed to a normal app boot's image entrypoint/cmd.
func isReleaseCommandGuest(guest mmds.Guest) bool {
	return len(guest.Entrypoint) > 0 && guest.Entrypoint[0] == "/bin/sh"
}

type fakeChecker struct {
	healthy bool
}

func (c *fakeChecker) Healthy(ctx context.Context, ip string, port int, path string) error {
	if c.healthy {
		return nil
	}
	return errors.New("not healthy")
}

// --- helpers ---------------------------------------------------------------

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testDeployer(t *testing.T, rt Runtime, chk Checker) *Deployer {
	t.Helper()
	return &Deployer{
		Store:        openTestStore(t),
		Runtime:      rt,
		Checker:      chk,
		DataDir:      t.TempDir(),
		LogDir:       t.TempDir(),
		EnsureBridge: fakeEnsureBridge,
	}
}

// fakeEnsureBridge stands in for the real netlink-backed
// Daemon.ensureBridge in tests: it never touches the network, just returns
// the bridge name/gateway idx would get in production.
func fakeEnsureBridge(idx int) (bridge, gateway string, err error) {
	return fmt.Sprintf("oak%d", idx), fmt.Sprintf("10.200.%d.1", idx), nil
}

func baseConfig(app string) *appconfig.Config {
	return &appconfig.Config{
		App: app,
		VM:  appconfig.VM{MemoryMB: 256, CPUs: 1},
	}
}

// --- Deploy: three required cases -----------------------------------------

func TestDeployHappyPathLeavesExactlyOneRunningMachine(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	id, err := d.Deploy(context.Background(), cfg, "localhost:5000/hello:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if id == "" {
		t.Fatal("empty machine id")
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	running := 0
	for _, m := range machines {
		if m.State == "running" {
			running++
		}
	}
	if running != 1 {
		t.Fatalf("want 1 running machine, got %d (%+v)", running, machines)
	}
}

func TestDeployFailingHealthCheckLeavesOldRunningAndDeletesNew(t *testing.T) {
	rt := &fakeRuntime{}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	d.HealthTimeout = 30 * time.Millisecond
	d.HealthInterval = 5 * time.Millisecond

	cfg := baseConfig("hello")
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}

	id1, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	d.Checker = &fakeChecker{healthy: false}
	_, err = d.Deploy(context.Background(), cfg, "img:2", nil)
	if err == nil {
		t.Fatal("want error from failing health check")
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("want exactly 1 machine left, got %d (%+v)", len(machines), machines)
	}
	if machines[0].ID != id1 || machines[0].State != "running" {
		t.Fatalf("old machine not left running: %+v", machines[0])
	}
}

func TestSecondDeployFreesOldIPForReuse(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("first deploy: %v", err)
	}
	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	ip1 := machines[0].IP

	if _, err := d.Deploy(context.Background(), cfg, "img:2", nil); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	freed, err := d.Store.AllocIP()
	if err != nil {
		t.Fatalf("alloc ip: %v", err)
	}
	if freed != ip1 {
		t.Fatalf("want freed ip %s reusable, got %s", ip1, freed)
	}
}

// --- volume attachment order -----------------------------------------------

func TestResolveVolumesOrdersByMountListAndAssignsDevices(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := d.Store.CreateVolume("hello", "data", "/var/lib/oak/volumes/hello-data.img", 5); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if err := d.Store.CreateVolume("hello", "cache", "/var/lib/oak/volumes/hello-cache.img", 1); err != nil {
		t.Fatalf("create volume: %v", err)
	}

	cfg := baseConfig("hello")
	cfg.Mounts = []appconfig.Mount{
		{Volume: "cache", Destination: "/cache"},
		{Volume: "data", Destination: "/data"},
	}

	volSpecs, mounts, err := d.resolveVolumes(cfg)
	if err != nil {
		t.Fatalf("resolve volumes: %v", err)
	}
	if len(volSpecs) != 2 || len(mounts) != 2 {
		t.Fatalf("got %d vol specs, %d mounts", len(volSpecs), len(mounts))
	}
	if volSpecs[0] != "/var/lib/oak/volumes/hello-cache.img" || mounts[0] != (mmds.Mount{Device: "/dev/vdb", Dest: "/cache"}) {
		t.Fatalf("first mount wrong: path=%s mount=%+v", volSpecs[0], mounts[0])
	}
	if volSpecs[1] != "/var/lib/oak/volumes/hello-data.img" || mounts[1] != (mmds.Mount{Device: "/dev/vdc", Dest: "/data"}) {
		t.Fatalf("second mount wrong: path=%s mount=%+v", volSpecs[1], mounts[1])
	}
}

func TestResolveVolumesErrorsOnUnknownVolume(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	cfg := baseConfig("hello")
	cfg.Mounts = []appconfig.Mount{{Volume: "nope", Destination: "/x"}}

	if _, _, err := d.resolveVolumes(cfg); err == nil {
		t.Fatal("want error for mount referencing unknown volume")
	}
}

// --- env merge with PORT rule -----------------------------------------------

func TestBuildGuestEnvDefaultsPortFromFirstService(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Env = map[string]string{"FOO": "bar"}
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}

	env, err := d.buildGuestEnv(cfg, []string{"BASE=1"})
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["PORT"] != "8080" {
		t.Fatalf("PORT not defaulted: %+v", env)
	}
	if env["FOO"] != "bar" {
		t.Fatalf("app env missing: %+v", env)
	}
	if env["BASE"] != "1" {
		t.Fatalf("image env missing: %+v", env)
	}
}

func TestBuildGuestEnvDoesNotClobberExplicitPortFromAppConfig(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Env = map[string]string{"PORT": "9090"}
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}

	env, err := d.buildGuestEnv(cfg, nil)
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["PORT"] != "9090" {
		t.Fatalf("clobbered explicit PORT: %+v", env)
	}
}

func TestBuildGuestEnvDoesNotClobberExplicitPortFromImage(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}

	env, err := d.buildGuestEnv(cfg, []string{"PORT=3000"})
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["PORT"] != "3000" {
		t.Fatalf("clobbered image PORT: %+v", env)
	}
}

func TestBuildGuestEnvSecretsWinOverAppConfigEnv(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	d.Identity = id

	ct, err := secrets.Encrypt(id.Recipient().String(), []byte("secretval"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if err := d.Store.PutSecret("hello", "FOO", ct); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	cfg := baseConfig("hello")
	cfg.Env = map[string]string{"FOO": "appval"}

	env, err := d.buildGuestEnv(cfg, nil)
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["FOO"] != "secretval" {
		t.Fatalf("secret should win over app config env: %+v", env)
	}
}

func TestBuildGuestEnvDefaultsPathWhenNotSetByImageOrConfig(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	env, err := d.buildGuestEnv(cfg, []string{"BASE=1"})
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["PATH"] != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Fatalf("PATH not defaulted: %+v", env)
	}
}

func TestBuildGuestEnvDoesNotClobberImagePath(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	env, err := d.buildGuestEnv(cfg, []string{"PATH=/opt/app/bin"})
	if err != nil {
		t.Fatalf("build guest env: %v", err)
	}
	if env["PATH"] != "/opt/app/bin" {
		t.Fatalf("clobbered image PATH: %+v", env)
	}
}

// --- Start's context must outlive the caller's per-request ctx ------------

func TestDeployStartCtxSurvivesRequestCancellation(t *testing.T) {
	rt := &fakeRuntime{}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	d.BaseCtx = context.Background()
	cfg := baseConfig("hello")

	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := d.Deploy(reqCtx, cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	cancel()

	rt.mu.Lock()
	startCtx := rt.startCtx
	rt.mu.Unlock()
	if startCtx == nil {
		t.Fatal("Start was never called")
	}
	if err := startCtx.Err(); err != nil {
		t.Fatalf("Start's ctx was cancelled along with the request ctx: %v", err)
	}
}

// --- watchMachine: reap an unexpectedly-exited machine ----------------------

func TestWatchMachineDeletesRowWhenProcessExits(t *testing.T) {
	waitCh := make(chan error, 1)
	rt := &fakeRuntime{waitCh: waitCh}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	id, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	close(waitCh)

	// watchMachine deletes the row outright rather than leaving it
	// "stopped": this design only ever holds an IP against a live "running"
	// row (see stopOldMachines/Deploy's own failure-cleanup paths), so a
	// lingering "stopped" row would leak both the row and its IP on every
	// unexpected exit.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := d.Store.Machine(id); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine %s row never deleted after exit", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWatchMachineNoopsWhenRowDeletedBeforeExit(t *testing.T) {
	waitCh := make(chan error, 1)
	rt := &fakeRuntime{waitCh: waitCh}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	id, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Simulate the row already having been superseded (e.g. by
	// stopOldMachines from a later deploy) before this machine's process
	// actually exits.
	if err := d.Store.DeleteMachine(id); err != nil {
		t.Fatalf("delete machine: %v", err)
	}
	close(waitCh)

	// watchMachine must neither panic nor resurrect the deleted row.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := d.Store.Machine(id); err == nil {
		t.Fatal("want machine to stay deleted")
	}
}

// --- Restart: reboot from latest release, no rebuild -----------------------

func TestRestartBootsFreshMachineFromLatestReleaseAndSupersedesOld(t *testing.T) {
	rt := &fakeRuntime{}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	id1, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	id2, err := d.Restart(context.Background(), "hello", nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if id2 == id1 {
		t.Fatal("restart should boot a new machine id")
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 1 || machines[0].ID != id2 || machines[0].State != "running" {
		t.Fatalf("want exactly the new machine running, got %+v", machines)
	}

	// Restart must not rebuild: exactly one BuildRootfs call (the original
	// deploy), and the rebooted machine reuses the same rootfs.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.starts) != 2 {
		t.Fatalf("want 2 Start calls (deploy + restart), got %d", len(rt.starts))
	}
	if rt.starts[1].RootFS != rt.starts[0].RootFS {
		t.Fatalf("restart used a different rootfs: %s vs %s", rt.starts[1].RootFS, rt.starts[0].RootFS)
	}
}

func TestRestartErrorsWhenAppHasNoRelease(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if _, err := d.Restart(context.Background(), "hello", nil); err == nil {
		t.Fatal("want error restarting an app with no release")
	}
}

// --- Scale: resize VM and reboot from latest release ------------------------

func TestScaleUpdatesVMSizeBootsFreshMachineAndSupersedesOld(t *testing.T) {
	rt := &fakeRuntime{}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	id1, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	id2, err := d.Scale(context.Background(), "hello", 512, 2, nil)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	if id2 == id1 {
		t.Fatal("scale should boot a new machine id")
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 1 || machines[0].ID != id2 || machines[0].State != "running" {
		t.Fatalf("want exactly the new machine running, got %+v", machines)
	}

	rt.mu.Lock()
	last := rt.starts[len(rt.starts)-1]
	rt.mu.Unlock()
	if last.MemoryMB != 512 || last.CPUs != 2 {
		t.Fatalf("scaled spec = %+v, want memory 512 cpus 2", last)
	}

	// The scaled size is persisted, so a later restart keeps it.
	raw, err := d.Store.AppConfig("hello")
	if err != nil {
		t.Fatalf("app config: %v", err)
	}
	if !strings.Contains(raw, `"MemoryMB":512`) || !strings.Contains(raw, `"CPUs":2`) {
		t.Fatalf("stored config missing scaled VM size: %s", raw)
	}
}

func TestScaleLeavesZeroFieldsUnchanged(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.VM = appconfig.VM{MemoryMB: 256, CPUs: 4}

	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if _, err := d.Scale(context.Background(), "hello", 1024, 0, nil); err != nil {
		t.Fatalf("scale: %v", err)
	}

	raw, err := d.Store.AppConfig("hello")
	if err != nil {
		t.Fatalf("app config: %v", err)
	}
	if !strings.Contains(raw, `"MemoryMB":1024`) || !strings.Contains(raw, `"CPUs":4`) {
		t.Fatalf("scale with cpus=0 should leave CPUs unchanged: %s", raw)
	}
}

// --- Destroy: full teardown --------------------------------------------------

func TestDestroyRemovesRowsFilesAndDropsTunnelRoute(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	tunnelConfig := filepath.Join(t.TempDir(), "config.yml")
	d.Domain = "example.com"
	d.TunnelID = "tid"
	d.TunnelConfig = tunnelConfig

	cfg := baseConfig("hello")
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}
	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	before, err := os.ReadFile(tunnelConfig)
	if err != nil {
		t.Fatalf("read tunnel config after deploy: %v", err)
	}
	if !strings.Contains(string(before), "hello.example.com") {
		t.Fatalf("tunnel config missing hello's route before destroy: %s", before)
	}

	if err := d.Destroy(context.Background(), "hello"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	after, err := os.ReadFile(tunnelConfig)
	if err != nil {
		t.Fatalf("read tunnel config after destroy: %v", err)
	}
	if strings.Contains(string(after), "hello.example.com") {
		t.Fatalf("tunnel config still has hello's route after destroy: %s", after)
	}

	if machines, err := d.Store.MachinesForApp("hello"); err != nil || len(machines) != 0 {
		t.Fatalf("machines after destroy = %+v, err %v, want none", machines, err)
	}
	if _, err := d.Store.LatestRelease("hello"); err == nil {
		t.Fatal("want releases gone after destroy")
	}
	if _, err := d.Store.AppConfig("hello"); err == nil {
		t.Fatal("want app row gone after destroy")
	}

	if _, err := os.Stat(filepath.Join(d.DataDir, "images", "hello")); !os.IsNotExist(err) {
		t.Fatalf("images dir for hello should be gone, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.LogDir, "hello")); !os.IsNotExist(err) {
		t.Fatalf("log dir for hello should be gone, stat err = %v", err)
	}
}

func TestApplyTunnelRoutesCustomDomainsToSameService(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	tunnelConfig := filepath.Join(t.TempDir(), "config.yml")
	d.Domain = "example.com"
	d.TunnelID = "tid"
	d.TunnelConfig = tunnelConfig

	cfg := baseConfig("hello")
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}
	cfg.Domains = []string{"misesnag.app", "www.misesnag.app"}
	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	raw, err := os.ReadFile(tunnelConfig)
	if err != nil {
		t.Fatalf("read tunnel config: %v", err)
	}

	machines, err := d.Store.RunningMachines()
	if err != nil {
		t.Fatalf("running machines: %v", err)
	}
	if len(machines) != 1 {
		t.Fatalf("machines = %+v, want exactly one", machines)
	}
	wantService := fmt.Sprintf("service: http://%s:8080", machines[0].IP)

	config := string(raw)
	for _, hostname := range []string{"hello.example.com", "misesnag.app", "www.misesnag.app"} {
		if !strings.Contains(config, "hostname: "+hostname) {
			t.Fatalf("tunnel config missing route for %s: %s", hostname, config)
		}
	}
	if n := strings.Count(config, wantService); n != 3 {
		t.Fatalf("want 3 routes pointing at %s (default + 2 custom domains), got %d: %s", wantService, n, config)
	}

	// Default hostname first, then custom domains in declared order, before
	// the oakberryio.<domain> catch-all.
	defaultIdx := strings.Index(config, "hostname: hello.example.com")
	firstIdx := strings.Index(config, "hostname: misesnag.app")
	secondIdx := strings.Index(config, "hostname: www.misesnag.app")
	oakIdx := strings.Index(config, "hostname: oakberryio.example.com")
	if !(defaultIdx < firstIdx && firstIdx < secondIdx && secondIdx < oakIdx) {
		t.Fatalf("route order wrong: default=%d misesnag=%d www=%d oak=%d\n%s", defaultIdx, firstIdx, secondIdx, oakIdx, config)
	}
}

func TestApplyTunnelIncludesStaticRoutes(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	tunnelConfig := filepath.Join(t.TempDir(), "config.yml")
	d.Domain = "example.com"
	d.TunnelID = "tid"
	d.TunnelConfig = tunnelConfig
	d.StaticRoutes = []tunnel.Route{{Hostname: "panel.example.com", Service: "http://127.0.0.1:4000"}}

	cfg := baseConfig("hello")
	cfg.Services = []appconfig.Service{{InternalPort: 8080}}
	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	raw, err := os.ReadFile(tunnelConfig)
	if err != nil {
		t.Fatalf("read tunnel config: %v", err)
	}
	config := string(raw)

	if !strings.Contains(config, "hostname: panel.example.com") {
		t.Fatalf("tunnel config missing static route hostname: %s", config)
	}
	if !strings.Contains(config, "service: http://127.0.0.1:4000") {
		t.Fatalf("tunnel config missing static route service: %s", config)
	}

	appIdx := strings.Index(config, "hostname: hello.example.com")
	staticIdx := strings.Index(config, "hostname: panel.example.com")
	oakIdx := strings.Index(config, "hostname: oakberryio.example.com")
	if !(appIdx < staticIdx && staticIdx < oakIdx) {
		t.Fatalf("route order wrong: app=%d static=%d oak=%d\n%s", appIdx, staticIdx, oakIdx, config)
	}
}

func TestDestroyLeavesOtherAppsUntouched(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})

	if _, err := d.Deploy(context.Background(), baseConfig("hello"), "img:1", nil); err != nil {
		t.Fatalf("deploy hello: %v", err)
	}
	survivorID, err := d.Deploy(context.Background(), baseConfig("other"), "img:1", nil)
	if err != nil {
		t.Fatalf("deploy other: %v", err)
	}

	if err := d.Destroy(context.Background(), "hello"); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	apps, err := d.Store.Apps()
	if err != nil {
		t.Fatalf("apps: %v", err)
	}
	if len(apps) != 1 || apps[0] != "other" {
		t.Fatalf("apps = %v, want just [other]", apps)
	}
	machines, err := d.Store.MachinesForApp("other")
	if err != nil || len(machines) != 1 || machines[0].ID != survivorID {
		t.Fatalf("other's machine = %+v, err %v, want just %s", machines, err, survivorID)
	}
}

// --- release_command ---------------------------------------------------

func TestDeployRunsReleaseCommandThenBootsApp(t *testing.T) {
	rt := &fakeRuntime{}
	rt.onStart = func(spec vm.Spec, guest mmds.Guest) chan error {
		if !isReleaseCommandGuest(guest) {
			return nil // the real app machine boots and blocks like normal
		}
		writeChildExitMarker(t, spec.LogPath, 0)
		return resolvedChan(nil)
	}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Deploy.ReleaseCommand = "/app/bin/migrate"

	id, err := d.Deploy(context.Background(), cfg, "img:1", nil)
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}

	rt.mu.Lock()
	numStarts := len(rt.starts)
	rt.mu.Unlock()
	if numStarts != 2 {
		t.Fatalf("want 2 Start calls (release command + app), got %d", numStarts)
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	// The release machine's row must be gone -- only the real app machine,
	// running, is left; the release machine never held a "running" row or
	// an IP past this function's return (see runReleaseCommand's doc
	// comment).
	if len(machines) != 1 {
		t.Fatalf("want exactly 1 machine row left (release machine cleaned up), got %d: %+v", len(machines), machines)
	}
	if machines[0].ID != id || machines[0].State != "running" {
		t.Fatalf("app machine not running: %+v", machines[0])
	}
}

func TestDeployFailingReleaseCommandBootsNoAppMachine(t *testing.T) {
	rt := &fakeRuntime{}
	rt.onStart = func(spec vm.Spec, guest mmds.Guest) chan error {
		if !isReleaseCommandGuest(guest) {
			return nil
		}
		writeChildExitMarker(t, spec.LogPath, 1)
		return resolvedChan(nil)
	}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Deploy.ReleaseCommand = "/app/bin/migrate"

	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err == nil {
		t.Fatal("want error from failing release command")
	} else if !strings.Contains(err.Error(), "/app/bin/migrate") {
		t.Fatalf("error should mention the release command: %v", err)
	}

	rt.mu.Lock()
	numStarts := len(rt.starts)
	rt.mu.Unlock()
	if numStarts != 1 {
		t.Fatalf("want only 1 Start call (release command only, app never booted), got %d", numStarts)
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 0 {
		t.Fatalf("want no leftover machine rows after a failed release command, got %+v", machines)
	}
}

func TestDeployReleaseCommandNoExitMarkerFailsDeploy(t *testing.T) {
	rt := &fakeRuntime{}
	rt.onStart = func(spec vm.Spec, guest mmds.Guest) chan error {
		if !isReleaseCommandGuest(guest) {
			return nil
		}
		// The VM "powers off" (Wait returns cleanly) but never printed
		// oak-init's marker -- e.g. it crashed before getting there.
		return resolvedChan(nil)
	}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")
	cfg.Deploy.ReleaseCommand = "/app/bin/migrate"

	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err == nil {
		t.Fatal("want error when no exit marker is found")
	}

	machines, err := d.Store.MachinesForApp("hello")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 0 {
		t.Fatalf("want no leftover machine rows, got %+v", machines)
	}
}

func TestReconcileDoesNotRunReleaseCommand(t *testing.T) {
	rt := &fakeRuntime{}
	deployer := testDeployer(t, rt, &fakeChecker{healthy: true})
	dm := &Daemon{Store: deployer.Store, Deployer: deployer}

	cfg := baseConfig("hello")
	cfg.Deploy.ReleaseCommand = "/app/bin/migrate"
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	if err := deployer.Store.UpsertApp(cfg.App, string(configJSON)); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	rootfsPath := filepath.Join(t.TempDir(), "img.ext4")
	cmdJSON, err := json.Marshal(releaseCmd{Entrypoint: []string{"/bin/app"}})
	if err != nil {
		t.Fatalf("marshal release cmd: %v", err)
	}
	envJSON, err := json.Marshal([]string{})
	if err != nil {
		t.Fatalf("marshal release env: %v", err)
	}
	releaseID, err := deployer.Store.InsertRelease(cfg.App, "img:1", rootfsPath, string(cmdJSON), string(envJSON), "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}

	id := "abcdef012345"
	if _, err := deployer.Store.AllocAndInsertMachine(id, cfg.App, releaseID, "oak-abcdef01", 0); err != nil {
		t.Fatalf("alloc and insert machine: %v", err)
	}
	if err := deployer.Store.SetMachineState(id, "running", 1234); err != nil {
		t.Fatalf("set machine state: %v", err)
	}
	m, err := deployer.Store.Machine(id)
	if err != nil {
		t.Fatalf("load machine: %v", err)
	}

	// reconcileOne must reboot the existing machine as-is, with no release
	// command run along the way even though cfg has one configured -- that
	// only ever happens from Deploy, never a reboot of an already-running
	// machine after a daemon restart.
	if err := dm.reconcileOne(context.Background(), m); err != nil {
		t.Fatalf("reconcileOne: %v", err)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.starts) != 1 {
		t.Fatalf("want exactly 1 Start call (no release-command boot), got %d", len(rt.starts))
	}
}

func TestRestartDoesNotRunReleaseCommand(t *testing.T) {
	rt := &fakeRuntime{}
	d := testDeployer(t, rt, &fakeChecker{healthy: true})
	cfg := baseConfig("hello")

	if _, err := d.Deploy(context.Background(), cfg, "img:1", nil); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// Set ReleaseCommand only after the initial deploy, directly on the
	// same *appconfig.Config Restart will read back via appConfig's cache
	// (Deploy caches the exact pointer passed in) -- Restart must not run
	// it even though it's now set, since only Deploy triggers a release
	// command.
	cfg.Deploy.ReleaseCommand = "/app/bin/migrate"

	if _, err := d.Restart(context.Background(), "hello", nil); err != nil {
		t.Fatalf("restart: %v", err)
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.starts) != 2 {
		t.Fatalf("want 2 Start calls total (initial deploy + restart, no release-command boot), got %d", len(rt.starts))
	}
}

package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/iagocavalcante/oakberryio/internal/appconfig"
	"github.com/iagocavalcante/oakberryio/internal/mmds"
	"github.com/iagocavalcante/oakberryio/internal/rootfs"
	"github.com/iagocavalcante/oakberryio/internal/secrets"
	"github.com/iagocavalcante/oakberryio/internal/store"
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextPID++
	r.starts = append(r.starts, spec)
	r.startCtx = ctx
	return &fakeHandle{pid: r.nextPID, waitCh: r.waitCh}, nil
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
		Store:   openTestStore(t),
		Runtime: rt,
		Checker: chk,
		DataDir: t.TempDir(),
		LogDir:  t.TempDir(),
	}
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

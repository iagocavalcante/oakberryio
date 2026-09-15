package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iagocavalcante/oakberryio/internal/metrics"
)

// TestAPIRejectsInvalidAppName covers HIGH #10: every handler reading
// {name} from the URL must reject a name that isn't a valid app name before
// using it to build a filesystem path or SQL lookup. handleMachines is
// exercised directly here; the other handlers share the exact same
// appconfig.ValidName guard added right after r.PathValue("name").
func TestAPIRejectsInvalidAppName(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}

	mux := api.Mux()
	req := httptest.NewRequest(http.MethodGet, "/apps/BadName/machines", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleScaleRejectsBothFieldsZero: a scale request that changes
// nothing would still reboot the app for no reason, so handleScale must
// reject it before calling Deployer.Scale.
func TestHandleScaleRejectsBothFieldsZero(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	mux := api.Mux()
	req := httptest.NewRequest(http.MethodPost, "/apps/hello/scale", strings.NewReader(`{"memory_mb":0,"cpus":0}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

// TestHandleListSecretsReturnsSortedKeysOnly ensures the wire response for
// GET .../secrets never leaks ciphertext or plaintext values.
func TestHandleListSecretsReturnsSortedKeysOnly(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}
	if err := d.Store.UpsertApp("hello", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := d.Store.PutSecret("hello", "ZKEY", []byte("super-secret-ciphertext")); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if err := d.Store.PutSecret("hello", "AKEY", []byte("another-ciphertext")); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	mux := api.Mux()
	req := httptest.NewRequest(http.MethodGet, "/apps/hello/secrets", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ciphertext") {
		t.Fatalf("response leaked secret value: %s", w.Body.String())
	}
	if w.Body.String() != `["AKEY","ZKEY"]`+"\n" {
		t.Fatalf("body = %q, want sorted key names only", w.Body.String())
	}
}

// insertRunningMachine records app's release+machine rows and marks the
// machine running with pid, for tests that need a running machine to show
// up in Store.RunningMachines() (handleMetrics' join).
func insertRunningMachine(t *testing.T, d *Deployer, app string, pid int) string {
	t.Helper()
	if err := d.Store.UpsertApp(app, "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	releaseID, err := d.Store.InsertRelease(app, "img:1", "/rootfs.ext4", "{}", "[]", "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	id := app + "-m1"
	if _, err := d.Store.AllocAndInsertMachine(id, app, releaseID, "oak-"+id, 0); err != nil {
		t.Fatalf("alloc and insert machine: %v", err)
	}
	if err := d.Store.SetMachineState(id, "running", pid); err != nil {
		t.Fatalf("set machine state: %v", err)
	}
	return id
}

// TestHandleDashboardIsExemptFromAuth: GET /dashboard must be reachable
// with no bearer token at all, since a plain browser navigation can't send
// one -- see AuthMiddleware's doc comment.
func TestHandleDashboardIsExemptFromAuth(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store, Token: "secret", Metrics: &metricsHolder{}}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	api.AuthMiddleware(api.Mux()).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "<title>") {
		t.Fatalf("body doesn't look like the dashboard page: %s", w.Body.String())
	}
}

// TestHandleMetricsRequiresAuth: unlike /dashboard, the data endpoint stays
// behind the bearer token even though both are served from the same mux.
func TestHandleMetricsRequiresAuth(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store, Token: "secret", Metrics: &metricsHolder{}}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	api.AuthMiddleware(api.Mux()).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestHandleMetricsJoinsStoreAndSnapshot exercises the wire shape of
// GET /metrics: host metrics from the published snapshot, and one entry per
// running machine joining the store's id/app/ip/pid/state with that
// machine's sampled cpu/mem from the snapshot.
func TestHandleMetricsJoinsStoreAndSnapshot(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	id := insertRunningMachine(t, d, "hello", 4321)

	mh := &metricsHolder{}
	mh.set(metrics.Snapshot{
		Host: metrics.HostMetrics{CPUPct: 12.5, VMCount: 1},
		Machines: map[string]metrics.MachineMetrics{
			id: {CPUPct: 33.3, MemBytes: 2048},
		},
	})
	api := &API{Deployer: d, Store: d.Store, Metrics: mh}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	api.Mux().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var body struct {
		Host     metrics.HostMetrics `json:"host"`
		Machines []metricsMachine    `json:"machines"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Host.CPUPct != 12.5 {
		t.Errorf("host cpu_pct = %v, want 12.5", body.Host.CPUPct)
	}
	if len(body.Machines) != 1 {
		t.Fatalf("machines = %+v, want exactly one", body.Machines)
	}
	m := body.Machines[0]
	if m.ID != id || m.App != "hello" || m.PID != 4321 || m.State != "running" {
		t.Errorf("machine = %+v, want id=%s app=hello pid=4321 state=running", m, id)
	}
	if m.CPUPct != 33.3 || m.MemBytes != 2048 {
		t.Errorf("machine metrics = %+v, want cpu_pct=33.3 mem_bytes=2048", m)
	}
}

// TestHandleAppsPlainReturnsStringSlice locks in the wire contract the CLI
// depends on (internal/cli/client.go's Client.Apps decodes a bare
// []string): adding owner to the store must not change GET /apps without
// ?detail=1.
func TestHandleAppsPlainReturnsStringSlice(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}
	if err := d.Store.UpsertApp("b", "{}"); err != nil {
		t.Fatalf("upsert app b: %v", err)
	}
	if err := d.Store.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app a: %v", err)
	}
	if err := d.Store.SetAppOwner("a", "alice"); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	mux := api.Mux()
	req := httptest.NewRequest(http.MethodGet, "/apps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if want := `["a","b"]` + "\n"; w.Body.String() != want {
		t.Fatalf("body = %q, want %q", w.Body.String(), want)
	}
}

// TestHandleAppsDetailReturnsOwners covers GET /apps?detail=1's wire shape:
// [{"name":"...","owner":"..."}], sorted by name, owner "" when unset.
func TestHandleAppsDetailReturnsOwners(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}
	if err := d.Store.UpsertApp("b", "{}"); err != nil {
		t.Fatalf("upsert app b: %v", err)
	}
	if err := d.Store.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app a: %v", err)
	}
	if err := d.Store.SetAppOwner("a", "alice"); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	mux := api.Mux()
	req := httptest.NewRequest(http.MethodGet, "/apps?detail=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if want := `[{"name":"a","owner":"alice"},{"name":"b","owner":""}]` + "\n"; w.Body.String() != want {
		t.Fatalf("body = %q, want %q", w.Body.String(), want)
	}
}

// TestHandleDeployRecordsOwnerOnCreateNotOnRedeploy exercises the create-only
// owner contract through the actual HTTP handler: a first deploy's "owner"
// field is recorded, and a redeploy with a different (or absent) owner
// leaves it untouched.
func TestHandleDeployRecordsOwnerOnCreateNotOnRedeploy(t *testing.T) {
	d := testDeployer(t, &fakeRuntime{waitCh: make(chan error)}, &fakeChecker{healthy: true})
	api := &API{Deployer: d, Store: d.Store}
	mux := api.Mux()

	deploy := func(body string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/apps/hello/deploy", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("deploy status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "\nok ") {
			t.Fatalf("deploy did not report ok: %s", w.Body.String())
		}
		return w.Body.String()
	}

	deploy(`{"image":"img:1","config":"app = \"hello\"","owner":"alice"}`)
	if owner, err := d.Store.AppOwner("hello"); err != nil || owner != "alice" {
		t.Fatalf("owner after create = %q, err %v, want alice", owner, err)
	}

	deploy(`{"image":"img:2","owner":"bob"}`)
	if owner, err := d.Store.AppOwner("hello"); err != nil || owner != "alice" {
		t.Fatalf("owner after redeploy = %q, err %v, want unchanged alice", owner, err)
	}
}

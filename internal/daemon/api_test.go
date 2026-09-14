package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

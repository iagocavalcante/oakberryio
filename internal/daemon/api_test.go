package daemon

import (
	"net/http"
	"net/http/httptest"
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

package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iagocavalcante/oakberryio/internal/appconfig"
	"github.com/iagocavalcante/oakberryio/internal/secrets"
	"github.com/iagocavalcante/oakberryio/internal/store"
)

// API wires oakd's HTTP surface: deploy, machine listing, logs, secrets,
// volumes and app listing. The same Mux is served on both the trusted unix
// socket and the bearer-token-guarded TCP listener; see daemon.go.
type API struct {
	Deployer *Deployer
	Store    *store.Store
	Token    string // bearer token required on the TCP listener; unix socket is trusted local access
}

// Mux builds the http.ServeMux shared by both listeners.
func (a *API) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apps/{name}/deploy", a.handleDeploy)
	mux.HandleFunc("GET /apps/{name}/machines", a.handleMachines)
	mux.HandleFunc("GET /apps/{name}/logs", a.handleLogs)
	mux.HandleFunc("PUT /apps/{name}/secrets", a.handleSecrets)
	mux.HandleFunc("POST /apps/{name}/volumes", a.handleCreateVolume)
	mux.HandleFunc("GET /apps", a.handleApps)
	return mux
}

// AuthMiddleware wraps next with a bearer-token check against a.Token. It's
// applied only to the TCP listener; the unix socket is served bare.
func (a *API) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleDeploy runs a deploy and streams progress lines to the client.
// Headers can't change once streaming starts, so the response is always
// 200 OK; the caller must parse the final line ("ok <machine-id>" or
// "error: <message>") for the real outcome.
//
// The plan's documented body is {"image": "..."}; that alone can't
// bootstrap a brand new app's first deploy (there's no oak.toml on file
// yet), so this also accepts an optional "config" field carrying the
// oak.toml text. A redeploy of an already-known app can omit it and reuse
// the last stored config -- see resolveConfig.
func (a *API) handleDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Image  string `json:"image"`
		Config string `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}
	if body.Image == "" {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}

	cfg, err := a.resolveConfig(name, body.Config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	progress := func(msg string) {
		fmt.Fprint(w, msg)
		if flusher != nil {
			flusher.Flush()
		}
	}

	id, err := a.Deployer.Deploy(r.Context(), cfg, body.Image, progress)
	if err != nil {
		progress(fmt.Sprintf("error: %v\n", err))
		return
	}
	progress(fmt.Sprintf("ok %s\n", id))
}

// resolveConfig parses tomlText as this app's oak.toml when given, else
// falls back to the app's last stored config.
func (a *API) resolveConfig(name, tomlText string) (*appconfig.Config, error) {
	if tomlText != "" {
		cfg, err := appconfig.Parse([]byte(tomlText))
		if err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
		if cfg.App != name {
			return nil, fmt.Errorf("config app %q does not match URL app %q", cfg.App, name)
		}
		return cfg, nil
	}

	raw, err := a.Store.AppConfig(name)
	if err != nil {
		return nil, fmt.Errorf("app %q has no stored config; first deploy must include \"config\": %w", name, err)
	}
	var cfg appconfig.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("decode stored config for %q: %w", name, err)
	}
	return &cfg, nil
}

func (a *API) handleMachines(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machines, err := a.Store.MachinesForApp(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(machines)
}

// handleLogs dumps the most recently created machine's log file. With
// ?follow=1 it tails the file until the client disconnects.
func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machines, err := a.Store.MachinesForApp(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(machines) == 0 {
		http.Error(w, "no machines for app "+name, http.StatusNotFound)
		return
	}
	// MachinesForApp orders by created_at ascending; the most recent is last.
	m := machines[len(machines)-1]
	logPath := filepath.Join(a.Deployer.LogDir, name, m.ID+".log")

	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)

	if r.URL.Query().Get("follow") != "1" {
		_, _ = io.Copy(w, f)
		return
	}

	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return
		}
	}
}

func (a *API) handleSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if a.Deployer.Identity == nil {
		http.Error(w, "no age identity loaded; cannot encrypt secrets", http.StatusInternalServerError)
		return
	}

	recipient := a.Deployer.Identity.Recipient().String()
	for key, value := range body {
		ct, err := secrets.Encrypt(recipient, []byte(value))
		if err != nil {
			http.Error(w, fmt.Sprintf("encrypt %s: %v", key, err), http.StatusInternalServerError)
			return
		}
		if err := a.Store.PutSecret(name, key, ct); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	var body struct {
		Name   string `json:"name"`
		SizeGB int    `json:"size_gb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Name == "" || body.SizeGB <= 0 {
		http.Error(w, "name and a positive size_gb are required", http.StatusBadRequest)
		return
	}

	dir := filepath.Join(a.Deployer.DataDir, "volumes")
	if err := os.MkdirAll(dir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.img", app, body.Name))

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	closeErr := f.Close()
	if closeErr != nil {
		http.Error(w, closeErr.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Truncate(path, int64(body.SizeGB)<<30); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := formatVolume(path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.Store.CreateVolume(app, body.Name, path, body.SizeGB); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (a *API) handleApps(w http.ResponseWriter, r *http.Request) {
	apps, err := a.Store.Apps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Strings(apps)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(apps)
}

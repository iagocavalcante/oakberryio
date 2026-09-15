package daemon

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	mux.HandleFunc("POST /apps/{name}/build", a.handleBuild)
	mux.HandleFunc("POST /apps/{name}/restart", a.handleRestart)
	mux.HandleFunc("POST /apps/{name}/scale", a.handleScale)
	mux.HandleFunc("DELETE /apps/{name}", a.handleDestroy)
	mux.HandleFunc("GET /apps/{name}/machines", a.handleMachines)
	mux.HandleFunc("GET /apps/{name}/logs", a.handleLogs)
	mux.HandleFunc("PUT /apps/{name}/secrets", a.handleSecrets)
	mux.HandleFunc("GET /apps/{name}/secrets", a.handleListSecrets)
	mux.HandleFunc("DELETE /apps/{name}/secrets", a.handleUnsetSecrets)
	mux.HandleFunc("POST /apps/{name}/volumes", a.handleCreateVolume)
	mux.HandleFunc("POST /apps/{name}/ssh", a.handleSSH)
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
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
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

// handleBuild builds and pushes app's image on the box -- the `oak deploy
// --remote` path, see docs/plans/2026-09-15-remote-build-design.md. The
// request body is a tar of the build context; the dockerfile path and
// build args travel as the X-Oak-Dockerfile and X-Oak-Build-Args headers
// (the latter a JSON object, possibly absent/empty for no build args).
//
// Everything that can still fail as a normal HTTP error -- a bad app name,
// unparsable headers, a malformed or path-escaping tar -- is checked before
// any output is written. Once the docker build starts, the contract matches
// handleDeploy: the response is always 200 OK and the real outcome is the
// final streamed line, "image <ref>" on success or "error: <message>" on
// failure.
func (a *API) handleBuild(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}

	dockerfile := r.Header.Get("X-Oak-Dockerfile")
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	buildArgs, err := parseBuildArgsHeader(r.Header.Get("X-Oak-Build-Args"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	buildRoot := filepath.Join(a.Deployer.DataDir, "build-tmp")
	if err := os.MkdirAll(buildRoot, 0755); err != nil {
		http.Error(w, fmt.Sprintf("mkdir build root: %v", err), http.StatusInternalServerError)
		return
	}
	ctxDir, err := os.MkdirTemp(buildRoot, "")
	if err != nil {
		http.Error(w, fmt.Sprintf("create build context dir: %v", err), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(ctxDir)

	if err := extractBuildContext(r.Body, ctxDir); err != nil {
		http.Error(w, fmt.Sprintf("extract build context: %v", err), http.StatusBadRequest)
		return
	}
	// dockerfile is as untrusted as any other tar entry name -- it travels
	// in a header instead of the tar, but a "-f ../../etc/passwd"-shaped
	// value would otherwise hand docker a path outside ctxDir, so it goes
	// through the exact same escape guard as every extracted file.
	dockerfilePath, err := safeExtractPath(ctxDir, dockerfile)
	if err != nil {
		http.Error(w, fmt.Sprintf("dockerfile: %v", err), http.StatusBadRequest)
		return
	}

	image := fmt.Sprintf("%s/%s:%d", a.Deployer.Registry, name, time.Now().UnixNano())
	dockerArgs := buildDockerBuildArgs(image, dockerfilePath, ctxDir, buildArgs)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fw := flushWriter{w: w, flusher: flusher}

	buildCmd := exec.CommandContext(r.Context(), "docker", dockerArgs...)
	buildCmd.Stdout = fw
	buildCmd.Stderr = fw
	if err := buildCmd.Run(); err != nil {
		fmt.Fprintf(fw, "error: docker build: %v\n", err)
		return
	}

	pushCmd := exec.CommandContext(r.Context(), "docker", "push", image)
	pushCmd.Stdout = fw
	pushCmd.Stderr = fw
	if err := pushCmd.Run(); err != nil {
		fmt.Fprintf(fw, "error: docker push: %v\n", err)
		return
	}

	fmt.Fprintf(fw, "image %s\n", image)
}

// handleRestart reboots app from its latest stored release, with no
// rebuild. Streams progress exactly like handleDeploy; see that handler's
// doc comment for the wire contract (always 200, final line is "ok
// <machine-id>" or "error: <message>").
func (a *API) handleRestart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
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

	id, err := a.Deployer.Restart(r.Context(), name, progress)
	if err != nil {
		progress(fmt.Sprintf("error: %v\n", err))
		return
	}
	progress(fmt.Sprintf("ok %s\n", id))
}

// handleScale resizes app's VM (memory and/or CPU count) and reboots it
// from its latest release, streaming progress the same way handleDeploy and
// handleRestart do. Rejects a request that leaves both fields zero -- that
// would be a no-op resize that still reboots the app for nothing.
func (a *API) handleScale(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
	var body struct {
		MemoryMB int `json:"memory_mb"`
		CPUs     int `json:"cpus"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("decode body: %v", err), http.StatusBadRequest)
		return
	}
	if body.MemoryMB == 0 && body.CPUs == 0 {
		http.Error(w, "at least one of memory_mb or cpus is required", http.StatusBadRequest)
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

	id, err := a.Deployer.Scale(r.Context(), name, body.MemoryMB, body.CPUs, progress)
	if err != nil {
		progress(fmt.Sprintf("error: %v\n", err))
		return
	}
	progress(fmt.Sprintf("ok %s\n", id))
}

// handleDestroy tears down app entirely: every machine, all its rows, its
// on-disk artifacts, and its tunnel route.
func (a *API) handleDestroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
	if err := a.Deployer.Destroy(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleMachines(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
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
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
	machines, err := a.Store.MachinesForApp(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(machines) == 0 {
		http.Error(w, "no machines for app "+name, http.StatusNotFound)
		return
	}
	// MachinesForApp orders by created_at descending; the most recent is first.
	m := machines[0]
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
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
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

// handleListSecrets returns the sorted key names of app's secrets, never
// their ciphertext or plaintext values.
func (a *API) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
	encrypted, err := a.Store.Secrets(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	keys := make([]string, 0, len(encrypted))
	for k := range encrypted {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(keys)
}

// handleUnsetSecrets deletes the named secret keys for app. Missing keys are
// silently ignored, matching DeleteSecret's own semantics.
func (a *API) handleUnsetSecrets(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}
	var body struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, key := range body.Keys {
		if err := a.Store.DeleteSecret(name, key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleCreateVolume(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("name")
	if !appconfig.ValidName(app) {
		http.Error(w, fmt.Sprintf("invalid app name %q", app), http.StatusBadRequest)
		return
	}
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
	// body.Name becomes part of a filesystem path below (dir/<app>-<name>.img);
	// reject anything that isn't a plain app-style name before it gets there.
	if !appconfig.ValidName(body.Name) {
		http.Error(w, fmt.Sprintf("invalid volume name %q", body.Name), http.StatusBadRequest)
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

// handleSSH bridges a raw duplex connection from the CLI to app's running
// machine over its Firecracker vsock device, for `oak ssh`. Unlike every
// other handler here, the response is a hijacked raw connection, not a
// normal HTTP response: see docs/plans/2026-09-14-ssh-and-stateful-design.md
// and internal/cli's Ssh client method for the wire contract on the other
// end.
//
// The vsock dial and CONNECT handshake happen before the hijack so a
// failure up to that point can still be reported as a normal HTTP error;
// once hijacked and the "200 OK" status line is on the wire, any further
// failure can only be reported as a line of text on the stream itself.
func (a *API) handleSSH(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !appconfig.ValidName(name) {
		http.Error(w, fmt.Sprintf("invalid app name %q", name), http.StatusBadRequest)
		return
	}

	machines, err := a.Store.MachinesForApp(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var running *store.Machine
	for i := range machines {
		if machines[i].State == "running" {
			running = &machines[i]
			break
		}
	}
	if running == nil {
		http.Error(w, "no running machine for app "+name, http.StatusNotFound)
		return
	}

	vsockConn, err := dialVsock(vsockSocketPath(a.Deployer.socketDir(), running.ID), sshAgentPort)
	if err != nil {
		http.Error(w, fmt.Sprintf("connect to guest agent: %v", err), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		vsockConn.Close()
		http.Error(w, "connection does not support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		vsockConn.Close()
		http.Error(w, fmt.Sprintf("hijack: %v", err), http.StatusInternalServerError)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		vsockConn.Close()
		clientConn.Close()
		return
	}

	bridge(clientConn, vsockConn)
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

# oakberryio Phase 1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** `oak deploy` takes a Dockerfile on the Mac and leaves a health-checked Firecracker microVM running on the Ubuntu box, reachable through Cloudflare Tunnel.

**Architecture:** One Go module, three binaries (`oakd` daemon, `oak` CLI, `oak-init` PID 1). SQLite is the only state. Firecracker is driven via `firecracker-go-sdk`. VM config (env, IP, mounts, cmd) reaches the guest through Firecracker MMDS, exactly like Fly's init. Design: `docs/plans/2026-09-08-oakberryio-design.md`.

**Tech Stack:** Go 1.22+, `modernc.org/sqlite`, `github.com/firecracker-microvm/firecracker-go-sdk`, `github.com/google/go-containerregistry`, `github.com/vishvananda/netlink`, `github.com/miekg/dns`, `github.com/BurntSushi/toml`, `filippo.io/age`. Host: Ubuntu Server 24.04, Firecracker v1.10+, cloudflared, `registry:2` via docker.

**Dev loop:** Unit tests run on the Mac (`go test ./...`). Anything touching `/dev/kvm`, taps, or `mkfs` is guarded by build tag `//go:build linux` and exercised only by `scripts/smoke.sh` on the box. Cross-compile with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, `scp` to the box, run.

**Conventions:**
- Module path `github.com/iagocavalcante/oakberryio`. Packages under `internal/`. Binaries under `cmd/`.
- Errors wrapped with `fmt.Errorf("context: %w", err)`. No panics outside `main`.
- Every table has `node_id TEXT NOT NULL DEFAULT 'local'`. Nothing else knows about nodes.
- Commit after every task with the session trailer:
  `Claude-Session: https://claude.ai/code/session_01CzG3KFGRSrANdjGEWnqiYc`

---

### Task 0: Host bootstrap script

**Files:**
- Create: `scripts/host-setup.sh`
- Create: `docs/host.md`

No unit test; verified once on the box.

**Step 1: Write the script**

```bash
#!/usr/bin/env bash
# Run as root on Ubuntu Server 24.04. Idempotent.
set -euo pipefail

FC_VERSION="${FC_VERSION:-v1.10.1}"
OAK_DOMAIN="${OAK_DOMAIN:?set OAK_DOMAIN=apps.example.com}"

grep -q -E 'svm|vmx' /proc/cpuinfo || { echo "no virtualization flag: enable SVM in BIOS"; exit 1; }
[ -e /dev/kvm ] || { echo "/dev/kvm missing"; exit 1; }

apt-get update
apt-get install -y curl jq nftables e2fsprogs docker.io age

# firecracker
if ! command -v firecracker >/dev/null; then
  arch=$(uname -m)
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${arch}.tgz" | tar xz -C /tmp
  install -m755 "/tmp/release-${FC_VERSION}-${arch}/firecracker-${FC_VERSION}-${arch}" /usr/local/bin/firecracker
fi

# guest kernel (Firecracker CI kernel, good enough for v1)
mkdir -p /var/lib/oak/{images,rootfs,volumes,kernel} /var/log/oak /etc/oak
[ -f /var/lib/oak/kernel/vmlinux ] || \
  curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-6.1.102" -o /var/lib/oak/kernel/vmlinux

# secrets key
[ -f /etc/oak/key ] || { age-keygen -o /etc/oak/key; chmod 600 /etc/oak/key; }

# bridge
cat >/etc/systemd/network/oak0.netdev <<'N'
[NetDev]
Name=oak0
Kind=bridge
N
cat >/etc/systemd/network/oak0.network <<'N'
[Match]
Name=oak0
[Network]
Address=10.200.0.1/16
IPForward=yes
N
systemctl enable --now systemd-networkd
networkctl reload

# nat
cat >/etc/nftables.conf <<'N'
flush ruleset
table ip nat {
  chain postrouting { type nat hook postrouting priority 100; oifname != "oak0" ip saddr 10.200.0.0/16 masquerade; }
}
N
systemctl enable --now nftables
nft -f /etc/nftables.conf
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/99-oak.conf

# local registry
docker ps -q -f name=oak-registry | grep -q . || \
  docker run -d --restart=always --name oak-registry -p 127.0.0.1:5000:5000 -v /var/lib/oak/registry:/var/lib/registry registry:2

# cloudflared
if ! command -v cloudflared >/dev/null; then
  curl -fsSL https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared
  chmod +x /usr/local/bin/cloudflared
fi
echo "DONE. Next: cloudflared tunnel login && cloudflared tunnel create oak, then put tunnel id in /etc/oak/oakd.toml"
```

**Step 2: Write docs/host.md** — the manual steps: BIOS SVM, run script, `cloudflared tunnel login/create`, DNS wildcard `*.${OAK_DOMAIN}` CNAME to the tunnel, `/etc/oak/oakd.toml` contents (see Task 8).

**Step 3: Commit**
```bash
git add scripts/host-setup.sh docs/host.md && git commit -m "feat: host bootstrap script"
```

---

### Task 1: Go module + `oak.toml` config

**Files:**
- Create: `go.mod`, `internal/appconfig/appconfig.go`, `internal/appconfig/appconfig_test.go`

**Step 1: Init module**
```bash
go mod init github.com/iagocavalcante/oakberryio && go get github.com/BurntSushi/toml
```

**Step 2: Failing test**
```go
package appconfig

import "testing"

const sample = `
app = "hello"
[build]
dockerfile = "Dockerfile"
[env]
PORT = "8080"
[[services]]
internal_port = 8080
[services.check]
path = "/health"
[[mounts]]
volume = "data"
destination = "/data"
[vm]
memory_mb = 256
cpus = 1
`

func TestParse(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil { t.Fatal(err) }
	if c.App != "hello" { t.Fatalf("app=%q", c.App) }
	if c.Services[0].InternalPort != 8080 { t.Fatal("port") }
	if c.Services[0].Check.Path != "/health" { t.Fatal("check") }
	if c.Mounts[0].Destination != "/data" { t.Fatal("mount") }
	if c.VM.MemoryMB != 256 { t.Fatal("mem") }
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`app = "x"`))
	if err != nil { t.Fatal(err) }
	if c.VM.MemoryMB != 256 || c.VM.CPUs != 1 { t.Fatal("defaults") }
}

func TestRejectsBadName(t *testing.T) {
	if _, err := Parse([]byte(`app = "Bad Name"`)); err == nil { t.Fatal("want error") }
}
```

**Step 3: Run** `go test ./internal/appconfig/` → FAIL (undefined Parse).

**Step 4: Implement**
```go
package appconfig

import (
	"fmt"
	"regexp"

	"github.com/BurntSushi/toml"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

type Config struct {
	App      string            `toml:"app"`
	Build    Build             `toml:"build"`
	Image    string            `toml:"image"`
	Env      map[string]string `toml:"env"`
	Services []Service         `toml:"services"`
	Mounts   []Mount           `toml:"mounts"`
	VM       VM                `toml:"vm"`
}
type Build struct{ Dockerfile string `toml:"dockerfile"` }
type Service struct {
	InternalPort int   `toml:"internal_port"`
	Check        Check `toml:"check"`
}
type Check struct{ Path string `toml:"path"` }
type Mount struct {
	Volume      string `toml:"volume"`
	Destination string `toml:"destination"`
}
type VM struct {
	MemoryMB int `toml:"memory_mb"`
	CPUs     int `toml:"cpus"`
}

func Parse(b []byte) (*Config, error) {
	c := &Config{VM: VM{MemoryMB: 256, CPUs: 1}}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse oak.toml: %w", err)
	}
	if !nameRe.MatchString(c.App) {
		return nil, fmt.Errorf("app name %q: lowercase letters, digits, dashes, max 32", c.App)
	}
	if c.Build.Dockerfile == "" { c.Build.Dockerfile = "Dockerfile" }
	return c, nil
}
```

**Step 5: Run** → PASS. **Step 6: Commit** `feat: oak.toml parser`.

---

### Task 2: SQLite store + IPAM

**Files:**
- Create: `internal/store/schema.sql`, `internal/store/store.go`, `internal/store/store_test.go`

**Step 1: Schema** (`schema.sql`, embedded)
```sql
CREATE TABLE IF NOT EXISTS apps (
  name TEXT PRIMARY KEY, node_id TEXT NOT NULL DEFAULT 'local',
  config TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS releases (
  id INTEGER PRIMARY KEY, app TEXT NOT NULL REFERENCES apps(name),
  image TEXT NOT NULL, rootfs TEXT NOT NULL, node_id TEXT NOT NULL DEFAULT 'local',
  cmd TEXT NOT NULL, env TEXT NOT NULL, workdir TEXT NOT NULL DEFAULT '/',
  created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY, app TEXT NOT NULL REFERENCES apps(name), release_id INTEGER NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', ip TEXT NOT NULL UNIQUE, tap TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('starting','running','stopped','failed')),
  pid INTEGER, created_at TEXT NOT NULL DEFAULT (datetime('now')));
CREATE TABLE IF NOT EXISTS volumes (
  name TEXT NOT NULL, app TEXT NOT NULL, path TEXT NOT NULL, size_gb INTEGER NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', PRIMARY KEY(app,name));
CREATE TABLE IF NOT EXISTS secrets (
  app TEXT NOT NULL, key TEXT NOT NULL, ciphertext BLOB NOT NULL,
  node_id TEXT NOT NULL DEFAULT 'local', PRIMARY KEY(app,key));
```

**Step 2: Failing tests**
```go
func TestIPAMAllocatesSequentialAndSkipsUsed(t *testing.T) {
	s := openTemp(t) // Open(":memory:")
	s.MustCreateApp("a", "{}")
	ip1, _ := s.AllocIP()
	if ip1 != "10.200.0.2" { t.Fatal(ip1) }
	s.MustInsertMachine("m1", "a", 1, ip1, "tap-m1")
	ip2, _ := s.AllocIP()
	if ip2 != "10.200.0.3" { t.Fatal(ip2) }
}
func TestMachinesForApp(t *testing.T) { /* insert two, list, expect two, states */ }
func TestSecretsRoundTrip(t *testing.T) { /* PutSecret then Secrets(app) returns map */ }
```

**Step 3: Run** → FAIL. **Step 4: Implement** `store.go`: `Open(path)`, `exec(schema)`, `AllocIP()` = smallest host in 10.200.0.0/16 from .2 not present in `machines.ip` (query all IPs, sort, scan; ponytail: O(n) scan, fine for < 60k). CRUD funcs used above. Use `modernc.org/sqlite` (`database/sql`, driver name `sqlite`), `PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON`.

**Step 5: Run** → PASS. **Step 6: Commit** `feat: sqlite store and IPAM`.

---

### Task 3: Image → rootfs builder

**Files:**
- Create: `internal/rootfs/flatten.go`, `internal/rootfs/flatten_test.go`, `internal/rootfs/mkfs_linux.go`, `internal/rootfs/mkfs_other.go`

Two halves: pure Go (pull image, flatten layers into a directory honoring whiteouts, extract image config) tested on Mac; `mkfs.ext4 -d dir out.ext4` Linux only.

**Step 1: Failing test** — build a fake image in-memory with `go-containerregistry`'s `random`/`mutate` packages: layer1 adds `/a` and `/dir/x`, layer2 adds `.wh.a` whiteout and `/b`. Flatten into `t.TempDir()`. Assert `/a` gone, `/b` and `/dir/x` present, and returned `ImageMeta{Cmd, Entrypoint, Env, WorkingDir}` matches config set via `mutate.Config`.

**Step 2: Run** → FAIL.

**Step 3: Implement**
```go
type ImageMeta struct {
	Entrypoint, Cmd, Env []string
	WorkingDir           string
}

// Flatten writes the image filesystem into dir and returns its runtime metadata.
func Flatten(img v1.Image, dir string) (*ImageMeta, error) {
	layers, _ := img.Layers()
	for _, l := range layers {
		rc, _ := l.Uncompressed()
		if err := untarLayer(rc, dir); err != nil { return nil, err }
		rc.Close()
	}
	cfg, _ := img.ConfigFile()
	return &ImageMeta{cfg.Config.Entrypoint, cfg.Config.Cmd, cfg.Config.Env, cfg.Config.WorkingDir}, nil
}
```
`untarLayer`: for each header, reject paths escaping `dir` (`filepath.Clean` + prefix check), handle `.wh..wh..opq` (remove dir contents) and `.wh.<name>` (remove), else write reg files/dirs/symlinks with mode. Hardlinks: `os.Link`.

`Pull(ref string) (v1.Image, error)` = `remote.Image(name.ParseReference(ref), remote.WithAuthFromKeychain(...))`. Registry is plaintext localhost, pass `name.Insecure`.

`mkfs_linux.go`: `func MakeExt4(dir, out string, sizeMB int) error` → `fallocate` file, `exec.Command("mkfs.ext4","-F","-d",dir,out)`. Also before mkfs copy `oak-init` binary to `dir/sbin/oak-init` (path from `OAK_INIT_BIN` config). `mkfs_other.go` returns `errors.New("linux only")`.

**Step 4: Run** → PASS. **Step 5: Commit** `feat: OCI image flatten to rootfs`.

---

### Task 4: MMDS payload contract

**Files:**
- Create: `internal/mmds/mmds.go`, `internal/mmds/mmds_test.go`

Shared between `oakd` (writes) and `oak-init` (reads). Keep it a plain struct, JSON.

```go
package mmds

type Guest struct {
	MachineID  string            `json:"machine_id"`
	App        string            `json:"app"`
	IP         string            `json:"ip"`       // "10.200.0.5/16"
	Gateway    string            `json:"gateway"`  // "10.200.0.1"
	DNS        string            `json:"dns"`      // "10.200.0.1"
	Env        map[string]string `json:"env"`
	Entrypoint []string          `json:"entrypoint"`
	Cmd        []string          `json:"cmd"`
	WorkingDir string            `json:"workdir"`
	Mounts     []Mount           `json:"mounts"`
}
type Mount struct {
	Device string `json:"device"`      // "/dev/vdb"
	Dest   string `json:"destination"` // "/data"
}
```
Test: JSON round-trip, and `Guest.Argv()` = entrypoint+cmd with error when both empty. Commit `feat: mmds guest contract`.

---

### Task 5: `oak-init` (guest PID 1)

**Files:**
- Create: `cmd/oak-init/main.go` (build tag linux), `cmd/oak-init/exec_test.go` (pure helpers)

Static binary: `CGO_ENABLED=0 GOOS=linux go build -ldflags '-s -w' ./cmd/oak-init`.

**Behavior, in order:**
1. `mount -t proc/sysfs/devtmpfs/tmpfs` on `/proc /sys /dev /tmp /run` (use `syscall.Mount`).
2. Bring `lo` up. Fetch `http://169.254.169.254/` with header `Accept: application/json` (MMDS v1; token dance for v2 is skipped, set `mmds-version: V1` on the host side). First configure `eth0` link up with a link-local? No: MMDS answers only on the tap, so: set eth0 up, add route `169.254.169.254 dev eth0`, GET, then apply real IP/gateway from payload via `netlink`.
3. Write `/etc/resolv.conf` = `nameserver <DNS>`, `/etc/hosts` with hostname = machine id.
4. For each mount: `mkfs.ext4` if blkid says no fs (needs `mkfs.ext4` in rootfs, so run it host-side instead: **decision: host formats volumes at create time**, init only mounts).
5. `chdir(WorkingDir)`, env = image Env + payload Env, exec `Argv()` as a child (not exec replacing PID 1; PID 1 must reap).
6. Forward SIGTERM/SIGINT to child, `wait4` loop reaping zombies. When child exits: sync, `reboot(LINUX_REBOOT_CMD_POWER_OFF)`. Exit code to serial before poweroff: `println("oak-init: exit", code)`.

Test the pure helpers: `mergeEnv(imageEnv []string, override map[string]string) []string` (override wins, stable order), `parseCIDR`. Commit `feat: oak-init guest init`.

---

### Task 6: Firecracker driver + tap networking

**Files:**
- Create: `internal/vm/vm_linux.go`, `internal/vm/vm_other.go`, `internal/vm/config.go`, `internal/vm/config_test.go`

Use `firecracker-go-sdk`. `config.go` (pure): builds `firecracker.Config` from a `Spec`:
```go
type Spec struct {
	ID        string
	Kernel    string   // /var/lib/oak/kernel/vmlinux
	RootFS    string
	Volumes   []string // extra drives, become /dev/vdb, /dev/vdc...
	Tap       string
	MAC       string   // derived: 06:00:<ip octets> — deterministic, test it
	MemoryMB  int64
	CPUs      int64
	LogPath   string   // serial console
	SocketDir string
}
```
Kernel args: `console=ttyS0 reboot=k panic=1 pci=off init=/sbin/oak-init`. Drives: root `is_root_device=true`, volumes not read-only. NIC: `AllowMMDS: true`. MMDS version V1, `MmdsAddress` default. Test: `BuildConfig(spec)` sets those fields, MAC derivation from IP `10.200.0.5` = `06:00:0a:c8:00:05`.

`vm_linux.go`:
```go
func Start(ctx context.Context, spec Spec, meta mmds.Guest) (*Machine, error)
```
- create tap `spec.Tap` with netlink (`Tuntap`, mode TAP), set master `oak0`, up.
- `firecracker.NewMachine(ctx, cfg, firecracker.WithLogger(...))`, `m.Start(ctx)`, `m.SetMetadata(ctx, meta)` **before** Start (SDK does it pre-boot when `Config.MmdsVersion` set — check SDK doc; otherwise use `m.Handlers.FcInit` hook).
- Return `Machine{PID, Stop(), Wait()}`. `Stop` = SendCtrlAltDel then kill after 10s.
- On any error: delete tap.

Commit `feat: firecracker driver`.

---

### Task 7: Internal DNS

**Files:** `internal/dns/dns.go`, `internal/dns/dns_test.go`

`miekg/dns` server on `10.200.0.1:53` UDP. Resolver func `func(app string) []net.IP` injected. Answers `A` for `<app>.internal.`, NXDOMAIN otherwise, forwards everything else to `1.1.1.1`. Test with an in-process server on `127.0.0.1:0` and `dns.Client`. Commit `feat: .internal dns`.

---

### Task 8: `oakd` daemon: config, API, deploy orchestration

**Files:**
- Create: `cmd/oakd/main.go`, `internal/daemon/daemon.go`, `internal/daemon/deploy.go`, `internal/daemon/deploy_test.go`, `internal/daemon/api.go`, `internal/tunnel/tunnel.go`, `internal/tunnel/tunnel_test.go`, `internal/secrets/secrets.go`, `internal/secrets/secrets_test.go`

`/etc/oak/oakd.toml`:
```toml
data_dir = "/var/lib/oak"
registry = "localhost:5000"
domain = "apps.example.com"
tunnel_id = "uuid"
tunnel_config = "/etc/cloudflared/config.yml"
key_file = "/etc/oak/key"
socket = "/run/oak.sock"
```

**secrets**: `Encrypt(pubkey, plaintext) []byte`, `Decrypt(identity, ct)`. age round-trip test.

**tunnel**: `Render(tunnelID, credsFile string, routes []Route) []byte` producing cloudflared YAML with `ingress:` entries `hostname: app.domain → service: http://ip:port`, final `- service: http_status:404`. Test: golden string. `Apply(path, bytes)` writes atomically and `systemctl reload cloudflared` (cloudflared reloads config on SIGHUP? It does not reliably; v1: `systemctl restart cloudflared`, ~2s blip. ponytail: restart, upgrade to Cloudflare API remote-managed ingress when the blip hurts).

**deploy.go** — the core. Interface-ize the two side-effecting deps so the flow is unit-testable:
```go
type Runtime interface {
	BuildRootfs(ctx, image string, out string) (*rootfs.ImageMeta, error)
	Start(ctx, vm.Spec, mmds.Guest) (Handle, error)
}
type Handle interface{ PID() int; Stop(ctx) error }
type Checker interface{ Healthy(ctx, ip string, port int, path string) error }

func (d *Deployer) Deploy(ctx, cfg *appconfig.Config, image string) (machineID string, err error)
```
Steps: upsert app → build rootfs → insert release → alloc IP → insert machine(starting) → Start → poll Checker up to 60s → on ok: stop previous running machines of app, mark stopped, mark new running, render+apply tunnel → return. On failure: Stop new, mark failed, return error with last 50 log lines.

**deploy_test.go**: fake Runtime/Checker. Cases: happy path leaves exactly one running machine; failing health leaves old machine running and new failed; second deploy frees old IP.

**api.go**: HTTP on unix socket. `POST /apps/{name}/deploy` (json: image), `GET /apps/{name}/machines`, `GET /apps/{name}/logs?follow=1` (tails file), `PUT /apps/{name}/secrets`, `POST /apps/{name}/volumes` (fallocate + `mkfs.ext4` on host). `GET /apps`.

**daemon.go**: wire config, store, dns server, runtime, api. `Reconcile()` on start: every machine with `state='running'` gets started again (new PID), failures marked failed. **Note:** on daemon restart the old firecracker processes are gone (children), so this is the restart path.

**main.go**: flag `-config`, run, handle SIGTERM → stop all machines gracefully.

Commit in three: `feat: secrets`, `feat: tunnel config`, `feat: oakd deploy + api`.

---

### Task 9: `oak` CLI

**Files:** `cmd/oak/main.go`, `internal/cli/*.go`

Std `flag` + subcommand switch, no cobra. Talks HTTP over unix socket (`OAK_SOCKET`, default `/run/oak.sock`) or `OAK_API=https://oak.<domain>` with bearer token from `~/.oak/token` (v1: token is a static string in `oakd.toml`, checked on the tunneled listener only; unix socket is trusted).

Commands:
- `oak deploy [-c oak.toml]`: parse config, `docker build -t <registry>/<app>:<unix-ts> .`, `docker push`, POST deploy, stream response lines.
- `oak status <app>`, `oak logs <app> [-f]`, `oak secrets set <app> K=V...`, `oak volumes create <app> <name> <size_gb>`, `oak apps`.

Remote build note: when running from the Mac, docker push must reach `localhost:5000` on the box. v1: `ssh -L 5000:localhost:5000 box` documented in `docs/host.md`. ponytail: port-forward, upgrade to tunneled registry route when annoying.

Test: `parseKV([]string{"A=1","B=2"})`. Commit `feat: oak cli`.

---

### Task 10: systemd units + smoke test

**Files:** `deploy/oakd.service`, `deploy/cloudflared.service` (if not from package), `scripts/smoke.sh`, `Makefile`, `examples/hello/{Dockerfile,main.go,oak.toml}`

`Makefile`: `build` (three binaries, linux/amd64 static), `install` (scp to `$BOX`, `systemctl restart oakd`).

`examples/hello`: Go HTTP server on `$PORT` returning hostname, `/health` 200.

`scripts/smoke.sh` (runs on the Mac, `BOX` and `OAK_DOMAIN` set):
```bash
set -euo pipefail
ssh -fN -L 5000:localhost:5000 "$BOX"
( cd examples/hello && OAK_API="https://oak.$OAK_DOMAIN" oak deploy )
oak status hello | grep running
curl -fsS "https://hello.$OAK_DOMAIN/health"
oak logs hello | grep -q "listening"
echo SMOKE OK
```
Run it. Fix until green. Commit `feat: systemd units, example app, smoke test`.

---

### Task 11: Backups timer

**Files:** `deploy/oak-backup.service`, `deploy/oak-backup.timer`, `scripts/backup.sh`

`backup.sh`: `rsync -a --sparse /var/lib/oak/volumes/ /var/lib/oak/backups/$(date +%F)/` plus `sqlite3 .backup` of `oak.db`; keep 7 days. Nightly timer. Commit `feat: nightly backups`.

---

## Done when
`scripts/smoke.sh` prints `SMOKE OK`, `systemctl restart oakd` brings `hello` back without a redeploy, and `oak deploy` a second time swaps the machine with the old one stopped.

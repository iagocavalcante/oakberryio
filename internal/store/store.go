// Package store is the sole state for oakd: apps, releases, machines,
// volumes and secrets, backed by SQLite.
package store

import (
	"database/sql"
	_ "embed"
	"encoding/binary"
	"fmt"
	"net"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store is the daemon's single SQLite-backed state store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// the schema. path may be ":memory:" for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// A single *sql.DB is shared by the whole daemon; modernc.org/sqlite
	// serializes access per-connection, so keep exactly one open connection
	// to avoid "database is locked" errors under WAL.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	// schemaSQL's CREATE TABLE IF NOT EXISTS is a no-op against a database
	// created before the apps.owner column existed, so back-fill it here.
	if err := addAppsOwnerColumn(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// addAppsOwnerColumn adds the apps.owner column if an older database
// doesn't have it yet. Idempotent: safe to call on every Open, including
// against a fresh database whose schemaSQL already declares the column.
func addAppsOwnerColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(apps)`)
	if err != nil {
		return fmt.Errorf("inspect apps schema: %w", err)
	}
	defer rows.Close()

	hasOwner := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan apps column: %w", err)
		}
		if name == "owner" {
			hasOwner = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate apps columns: %w", err)
	}
	if hasOwner {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE apps ADD COLUMN owner TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add apps.owner column: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertApp creates the app or replaces its stored config if it already
// exists.
func (s *Store) UpsertApp(name, configJSON string) error {
	_, err := s.db.Exec(
		`INSERT INTO apps(name, config) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET config = excluded.config`,
		name, configJSON,
	)
	if err != nil {
		return fmt.Errorf("upsert app %s: %w", name, err)
	}
	return nil
}

// Apps lists every registered app name, sorted.
func (s *Store) Apps() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()

	var apps []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate apps: %w", err)
	}
	return apps, nil
}

// AppConfig returns the config most recently stored for app by UpsertApp.
func (s *Store) AppConfig(name string) (string, error) {
	var config string
	err := s.db.QueryRow(`SELECT config FROM apps WHERE name = ?`, name).Scan(&config)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("app config for %s: not found", name)
		}
		return "", fmt.Errorf("app config for %s: %w", name, err)
	}
	return config, nil
}

// SetAppOwner records app's owner, but only if it doesn't have one yet.
// Ownership is create-time only: once set, a later call (e.g. a redeploy
// with a different owner) is a silent no-op, so an app's owner never
// changes after its first deploy.
func (s *Store) SetAppOwner(name, owner string) error {
	if _, err := s.db.Exec(`UPDATE apps SET owner = ? WHERE name = ? AND owner = ''`, owner, name); err != nil {
		return fmt.Errorf("set owner for app %s: %w", name, err)
	}
	return nil
}

// AppOwner returns app's owner, or "" if it has none.
func (s *Store) AppOwner(name string) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT owner FROM apps WHERE name = ?`, name).Scan(&owner)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("app owner for %s: not found", name)
		}
		return "", fmt.Errorf("app owner for %s: %w", name, err)
	}
	return owner, nil
}

// AppInfo is one row of an app's name and owner, as returned by
// AppsDetailed. Owner is "" for an app deployed without one.
type AppInfo struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// AppsDetailed lists every registered app with its owner, sorted by name.
// Apps returns the bare name list the CLI depends on; this is the version
// listings that need ownership (e.g. the frontend panel) use instead.
func (s *Store) AppsDetailed() ([]AppInfo, error) {
	rows, err := s.db.Query(`SELECT name, owner FROM apps ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list apps detailed: %w", err)
	}
	defer rows.Close()

	var apps []AppInfo
	for rows.Next() {
		var info AppInfo
		if err := rows.Scan(&info.Name, &info.Owner); err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate apps: %w", err)
	}
	return apps, nil
}

// InsertRelease records a new release for app and returns its id.
func (s *Store) InsertRelease(app, image, rootfs, cmdJSON, envJSON, workdir string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO releases(app, image, rootfs, cmd, env, workdir) VALUES (?, ?, ?, ?, ?, ?)`,
		app, image, rootfs, cmdJSON, envJSON, workdir,
	)
	if err != nil {
		return 0, fmt.Errorf("insert release for %s: %w", app, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("release id for %s: %w", app, err)
	}
	return id, nil
}

// ipamMaxHost is the top of a /24's usable host range: .2-.254 (.1 is the
// gateway, .0/.255 are network/broadcast).
const ipamMaxHost = 254

// tenantSubnetMax is the largest tenant subnet index SubnetForOwner will
// hand out: idx 0 is the admin/legacy subnet (seeded in schema.sql), so
// 1..255 leaves 255 tenants -- a bridge name "oak255" is still well within
// the 15-char Linux ifname limit. Ample for a homelab; see the design doc.
const tenantSubnetMax = 255

// subnetBase returns the dotted-quad base address of tenant subnet idx's
// /24, e.g. idx 3 -> "10.200.3.0".
func subnetBase(idx int) string {
	return fmt.Sprintf("10.200.%d.0", idx)
}

// AllocIP returns the smallest free host address in the admin subnet
// (10.200.0.0/24), starting at 10.200.0.2 (10.200.0.1 is reserved for the
// gateway).
//
// ponytail: O(n) scan over allocated machine IPs on every call; fine up to
// tens of thousands of machines, revisit with a free-list if it shows up in
// profiles.
func (s *Store) AllocIP() (string, error) {
	return allocIP(s.db, 0)
}

// dbExecer is satisfied by both *sql.DB and *sql.Tx, so allocIP and
// insertMachine can run either standalone (existing callers, existing
// tests) or sequentially inside one BEGIN IMMEDIATE/COMMIT (see
// AllocAndInsertMachine) without duplicating either query.
type dbExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// allocIP returns the smallest free host address in tenant subnet idx's
// /24 (10.200.<idx>.0/24), scanning every allocated machine IP but only
// treating the ones inside that /24 as used -- so each tenant's IPAM is
// independent of every other tenant's.
func allocIP(db dbExecer, idx int) (string, error) {
	rows, err := db.Query(`SELECT ip FROM machines`)
	if err != nil {
		return "", fmt.Errorf("query allocated ips: %w", err)
	}
	defer rows.Close()

	base, err := ipToUint32(subnetBase(idx))
	if err != nil {
		return "", fmt.Errorf("parse subnet base for idx %d: %w", idx, err)
	}

	used := make(map[uint32]bool)
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return "", fmt.Errorf("scan ip: %w", err)
		}
		addr, err := ipToUint32(ip)
		if err != nil {
			return "", fmt.Errorf("parse allocated ip %q: %w", ip, err)
		}
		if addr&0xFFFFFF00 == base {
			used[addr] = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate allocated ips: %w", err)
	}

	for host := uint32(2); host <= ipamMaxHost; host++ {
		candidate := base | host
		if !used[candidate] {
			return uint32ToIP(candidate), nil
		}
	}
	return "", fmt.Errorf("no free ip in %s/24", subnetBase(idx))
}

// SubnetForOwner returns owner's tenant subnet index, allocating the
// smallest free index >= 1 the first time owner is seen. The admin/legacy
// owner "" is seeded at idx 0 by schema.sql, so this is only ever called
// with a non-"" owner in practice, but works uniformly for any owner.
//
// The lookup-or-allocate is one BEGIN/COMMIT transaction so a concurrent
// call for a different new owner can never observe the
// allocated-but-not-yet-inserted gap and hand out the same index twice --
// same reasoning as AllocAndInsertMachine, and likewise relying on Store's
// single underlying connection (SetMaxOpenConns(1) in Open) to make plain
// sequential Exec/Query calls between Begin and Commit correct here.
func (s *Store) SubnetForOwner(owner string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin subnet for owner %q: %w", owner, err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	var idx int
	err = tx.QueryRow(`SELECT idx FROM tenant_subnets WHERE owner = ?`, owner).Scan(&idx)
	if err == nil {
		return idx, tx.Commit()
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("lookup subnet for owner %q: %w", owner, err)
	}

	idx, err = smallestFreeSubnetIdx(tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO tenant_subnets(owner, idx) VALUES (?, ?)`, owner, idx); err != nil {
		return 0, fmt.Errorf("insert subnet for owner %q: %w", owner, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit subnet for owner %q: %w", owner, err)
	}
	return idx, nil
}

// smallestFreeSubnetIdx scans tenant_subnets for the smallest unused index
// in [1, tenantSubnetMax].
//
// ponytail: O(n) scan, same tradeoff as allocIP -- fine for the handful of
// tenant subnets this design supports.
func smallestFreeSubnetIdx(db dbExecer) (int, error) {
	rows, err := db.Query(`SELECT idx FROM tenant_subnets`)
	if err != nil {
		return 0, fmt.Errorf("query tenant subnets: %w", err)
	}
	defer rows.Close()

	used := make(map[int]bool)
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return 0, fmt.Errorf("scan tenant subnet: %w", err)
		}
		used[idx] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate tenant subnets: %w", err)
	}

	for idx := 1; idx <= tenantSubnetMax; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("no free tenant subnet (max %d)", tenantSubnetMax)
}

// OwnerForSubnet returns the owner tenant subnet idx belongs to -- the
// reverse of SubnetForOwner. Used by the DNS resolver to translate a
// query's source IP (whose third octet is the tenant subnet index) into
// the tenant it belongs to.
func (s *Store) OwnerForSubnet(idx int) (string, error) {
	var owner string
	err := s.db.QueryRow(`SELECT owner FROM tenant_subnets WHERE idx = ?`, idx).Scan(&owner)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("tenant subnet %d: not found", idx)
		}
		return "", fmt.Errorf("owner for tenant subnet %d: %w", idx, err)
	}
	return owner, nil
}

func ipToUint32(s string) (uint32, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, fmt.Errorf("invalid ip %q", s)
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, fmt.Errorf("not an ipv4 address: %q", s)
	}
	return binary.BigEndian.Uint32(v4), nil
}

func uint32ToIP(v uint32) string {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, v)
	return b.String()
}

// Machine is a row from the machines table.
type Machine struct {
	ID        string
	App       string
	ReleaseID int64
	NodeID    string
	IP        string
	Tap       string
	State     string
	PID       int64
	CreatedAt string
}

// InsertMachine records a new machine in the "starting" state.
func (s *Store) InsertMachine(id, app string, releaseID int64, ip, tap string) error {
	return insertMachine(s.db, id, app, releaseID, ip, tap)
}

func insertMachine(db dbExecer, id, app string, releaseID int64, ip, tap string) error {
	_, err := db.Exec(
		`INSERT INTO machines(id, app, release_id, ip, tap, state) VALUES (?, ?, ?, ?, ?, 'starting')`,
		id, app, releaseID, ip, tap,
	)
	if err != nil {
		return fmt.Errorf("insert machine %s: %w", id, err)
	}
	return nil
}

// AllocAndInsertMachine allocates a free IP in tenant subnet idx and
// inserts the new machine row (state "starting") in one BEGIN
// IMMEDIATE/COMMIT transaction, so a concurrent AllocIP can never observe
// the allocated-but-not-yet-inserted gap between the two calls and hand out
// the same address twice. Store already serializes all access through a
// single *sql.DB connection (SetMaxOpenConns(1) in Open), so plain
// sequential Exec/Query calls between BEGIN IMMEDIATE and COMMIT are
// correct here, matching the rest of this file's non-transactional style.
func (s *Store) AllocAndInsertMachine(id, app string, releaseID int64, tap string, idx int) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("begin alloc+insert machine %s: %w", id, err)
	}
	defer tx.Rollback() // no-op once Commit has succeeded

	ip, err := allocIP(tx, idx)
	if err != nil {
		return "", err
	}
	if err := insertMachine(tx, id, app, releaseID, ip, tap); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit alloc+insert machine %s: %w", id, err)
	}
	return ip, nil
}

// SetMachineState transitions a machine to state and records its host pid.
// Pass 0 for pid when the machine has none (e.g. state "failed").
func (s *Store) SetMachineState(id, state string, pid int) error {
	var pidArg any
	if pid != 0 {
		pidArg = pid
	}
	res, err := s.db.Exec(`UPDATE machines SET state = ?, pid = ? WHERE id = ?`, state, pidArg, id)
	if err != nil {
		return fmt.Errorf("set machine %s state: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("machine %s: not found", id)
	}
	return nil
}

// DeleteMachine removes a machine, freeing its IP for reallocation.
func (s *Store) DeleteMachine(id string) error {
	if _, err := s.db.Exec(`DELETE FROM machines WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete machine %s: %w", id, err)
	}
	return nil
}

// MachinesForApp lists all machines belonging to app, most recently created
// first.
func (s *Store) MachinesForApp(app string) ([]Machine, error) {
	rows, err := s.db.Query(
		`SELECT id, app, release_id, node_id, ip, tap, state, COALESCE(pid, 0), created_at
		 FROM machines WHERE app = ? ORDER BY created_at DESC, rowid DESC`,
		app,
	)
	if err != nil {
		return nil, fmt.Errorf("machines for app %s: %w", app, err)
	}
	return scanMachines(rows)
}

// RunningMachines lists every machine in the "running" state, across all
// apps, most recently created first. Used by the daemon's startup reconcile
// loop.
func (s *Store) RunningMachines() ([]Machine, error) {
	rows, err := s.db.Query(
		`SELECT id, app, release_id, node_id, ip, tap, state, COALESCE(pid, 0), created_at
		 FROM machines WHERE state = 'running' ORDER BY created_at DESC, rowid DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("running machines: %w", err)
	}
	return scanMachines(rows)
}

// Machine looks up a single machine by id, e.g. for the post-Wait
// still-current-row check in the daemon's background reap goroutine.
func (s *Store) Machine(id string) (Machine, error) {
	rows, err := s.db.Query(
		`SELECT id, app, release_id, node_id, ip, tap, state, COALESCE(pid, 0), created_at
		 FROM machines WHERE id = ?`,
		id,
	)
	if err != nil {
		return Machine{}, fmt.Errorf("machine %s: %w", id, err)
	}
	machines, err := scanMachines(rows)
	if err != nil {
		return Machine{}, err
	}
	if len(machines) == 0 {
		return Machine{}, fmt.Errorf("machine %s: not found", id)
	}
	return machines[0], nil
}

func scanMachines(rows *sql.Rows) ([]Machine, error) {
	defer rows.Close()
	var machines []Machine
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.ID, &m.App, &m.ReleaseID, &m.NodeID, &m.IP, &m.Tap, &m.State, &m.PID, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan machine: %w", err)
		}
		machines = append(machines, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate machines: %w", err)
	}
	return machines, nil
}

// PutSecret stores (or overwrites) an already-encrypted secret value.
func (s *Store) PutSecret(app, key string, ciphertext []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO secrets(app, key, ciphertext) VALUES (?, ?, ?)
		 ON CONFLICT(app, key) DO UPDATE SET ciphertext = excluded.ciphertext`,
		app, key, ciphertext,
	)
	if err != nil {
		return fmt.Errorf("put secret %s/%s: %w", app, key, err)
	}
	return nil
}

// Secrets returns all secrets for app, keyed by name, still encrypted.
func (s *Store) Secrets(app string) (map[string][]byte, error) {
	rows, err := s.db.Query(`SELECT key, ciphertext FROM secrets WHERE app = ?`, app)
	if err != nil {
		return nil, fmt.Errorf("secrets for app %s: %w", app, err)
	}
	defer rows.Close()

	secrets := make(map[string][]byte)
	for rows.Next() {
		var key string
		var ciphertext []byte
		if err := rows.Scan(&key, &ciphertext); err != nil {
			return nil, fmt.Errorf("scan secret: %w", err)
		}
		secrets[key] = ciphertext
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secrets: %w", err)
	}
	return secrets, nil
}

// Release is a row from the releases table.
type Release struct {
	ID        int64
	App       string
	Image     string
	RootFS    string
	NodeID    string
	Cmd       string
	Env       string
	Workdir   string
	CreatedAt string
}

// ReleaseByID looks up a single release, e.g. to rebuild a machine's VM spec
// during the daemon's startup reconcile.
func (s *Store) ReleaseByID(id int64) (Release, error) {
	var r Release
	err := s.db.QueryRow(
		`SELECT id, app, image, rootfs, node_id, cmd, env, workdir, created_at
		 FROM releases WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.App, &r.Image, &r.RootFS, &r.NodeID, &r.Cmd, &r.Env, &r.Workdir, &r.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Release{}, fmt.Errorf("release %d: not found", id)
		}
		return Release{}, fmt.Errorf("release %d: %w", id, err)
	}
	return r, nil
}

// LatestRelease returns the most recently created release for app.
func (s *Store) LatestRelease(app string) (Release, error) {
	var r Release
	err := s.db.QueryRow(
		`SELECT id, app, image, rootfs, node_id, cmd, env, workdir, created_at
		 FROM releases WHERE app = ? ORDER BY id DESC LIMIT 1`,
		app,
	).Scan(&r.ID, &r.App, &r.Image, &r.RootFS, &r.NodeID, &r.Cmd, &r.Env, &r.Workdir, &r.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Release{}, fmt.Errorf("latest release for %s: not found", app)
		}
		return Release{}, fmt.Errorf("latest release for %s: %w", app, err)
	}
	return r, nil
}

// DeleteApp removes app's row. Callers must delete its releases first (the
// releases table has a foreign key on apps.name).
func (s *Store) DeleteApp(name string) error {
	if _, err := s.db.Exec(`DELETE FROM apps WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete app %s: %w", name, err)
	}
	return nil
}

// DeleteReleasesForApp removes every release row for app.
func (s *Store) DeleteReleasesForApp(app string) error {
	if _, err := s.db.Exec(`DELETE FROM releases WHERE app = ?`, app); err != nil {
		return fmt.Errorf("delete releases for %s: %w", app, err)
	}
	return nil
}

// DeleteSecretsForApp removes every secret for app.
func (s *Store) DeleteSecretsForApp(app string) error {
	if _, err := s.db.Exec(`DELETE FROM secrets WHERE app = ?`, app); err != nil {
		return fmt.Errorf("delete secrets for %s: %w", app, err)
	}
	return nil
}

// DeleteVolumesForApp removes every volume row for app. The backing .img
// file is removed separately by the caller (Deployer.Destroy).
func (s *Store) DeleteVolumesForApp(app string) error {
	if _, err := s.db.Exec(`DELETE FROM volumes WHERE app = ?`, app); err != nil {
		return fmt.Errorf("delete volumes for %s: %w", app, err)
	}
	return nil
}

// DeleteSecret removes a single secret, e.g. for `oak secrets unset`.
func (s *Store) DeleteSecret(app, key string) error {
	if _, err := s.db.Exec(`DELETE FROM secrets WHERE app = ? AND key = ?`, app, key); err != nil {
		return fmt.Errorf("delete secret %s/%s: %w", app, key, err)
	}
	return nil
}

// Volume is a row from the volumes table.
type Volume struct {
	Name   string
	App    string
	Path   string
	SizeGB int
	NodeID string
}

// CreateVolume records a new volume for app.
func (s *Store) CreateVolume(app, name, path string, sizeGB int) error {
	_, err := s.db.Exec(
		`INSERT INTO volumes(app, name, path, size_gb) VALUES (?, ?, ?, ?)`,
		app, name, path, sizeGB,
	)
	if err != nil {
		return fmt.Errorf("create volume %s/%s: %w", app, name, err)
	}
	return nil
}

// Volumes lists all volumes for app.
func (s *Store) Volumes(app string) ([]Volume, error) {
	rows, err := s.db.Query(`SELECT name, app, path, size_gb, node_id FROM volumes WHERE app = ?`, app)
	if err != nil {
		return nil, fmt.Errorf("volumes for app %s: %w", app, err)
	}
	defer rows.Close()

	var volumes []Volume
	for rows.Next() {
		var v Volume
		if err := rows.Scan(&v.Name, &v.App, &v.Path, &v.SizeGB, &v.NodeID); err != nil {
			return nil, fmt.Errorf("scan volume: %w", err)
		}
		volumes = append(volumes, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate volumes: %w", err)
	}
	return volumes, nil
}

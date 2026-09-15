package store

import (
	"database/sql"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIPAMAllocatesSequentialAndSkipsUsed(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	relID, err := s.InsertRelease("a", "img:1", "/var/lib/oak/rootfs/a-1.ext4", `["/bin/app"]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}

	ip1, err := s.AllocIP()
	if err != nil {
		t.Fatalf("alloc ip: %v", err)
	}
	if ip1 != "10.200.0.2" {
		t.Fatalf("ip1 = %q, want 10.200.0.2", ip1)
	}

	if err := s.InsertMachine("m1", "a", relID, ip1, "tap-m1"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}

	ip2, err := s.AllocIP()
	if err != nil {
		t.Fatalf("alloc ip: %v", err)
	}
	if ip2 != "10.200.0.3" {
		t.Fatalf("ip2 = %q, want 10.200.0.3", ip2)
	}
}

func TestAllocIPFillsGapAfterMachineRemoved(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	relID, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `[]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	ip1, _ := s.AllocIP()
	if err := s.InsertMachine("m1", "a", relID, ip1, "tap-m1"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	ip2, _ := s.AllocIP()
	if err := s.InsertMachine("m2", "a", relID, ip2, "tap-m2"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	if err := s.SetMachineState("m1", "stopped", 0); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if err := s.DeleteMachine("m1"); err != nil {
		t.Fatalf("delete machine: %v", err)
	}
	ip3, err := s.AllocIP()
	if err != nil {
		t.Fatalf("alloc ip: %v", err)
	}
	if ip3 != ip1 {
		t.Fatalf("ip3 = %q, want reclaimed %q", ip3, ip1)
	}
}

func TestAllocAndInsertMachineAllocatesAndInsertsAtomically(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	relID, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `[]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}

	ip1, err := s.AllocAndInsertMachine("m1", "a", relID, "tap-m1")
	if err != nil {
		t.Fatalf("alloc and insert machine: %v", err)
	}
	if ip1 != "10.200.0.2" {
		t.Fatalf("ip1 = %q, want 10.200.0.2", ip1)
	}

	// The insert must be visible immediately (the transaction committed),
	// and AllocIP must see it and skip it.
	machines, err := s.MachinesForApp("a")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 1 || machines[0].ID != "m1" || machines[0].IP != ip1 {
		t.Fatalf("machines = %+v, want one row m1/%s", machines, ip1)
	}

	ip2, err := s.AllocAndInsertMachine("m2", "a", relID, "tap-m2")
	if err != nil {
		t.Fatalf("alloc and insert machine 2: %v", err)
	}
	if ip2 != "10.200.0.3" {
		t.Fatalf("ip2 = %q, want 10.200.0.3 (m1's ip must not be reallocated)", ip2)
	}
}

func TestMachinesForApp(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	relID, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `[]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	ip1, _ := s.AllocIP()
	if err := s.InsertMachine("m1", "a", relID, ip1, "tap-m1"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	ip2, _ := s.AllocIP()
	if err := s.InsertMachine("m2", "a", relID, ip2, "tap-m2"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	if err := s.SetMachineState("m2", "running", 4242); err != nil {
		t.Fatalf("set state: %v", err)
	}

	machines, err := s.MachinesForApp("a")
	if err != nil {
		t.Fatalf("machines for app: %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("len(machines) = %d, want 2", len(machines))
	}
	var m2 *Machine
	for i := range machines {
		if machines[i].ID == "m2" {
			m2 = &machines[i]
		}
	}
	if m2 == nil {
		t.Fatal("m2 not found")
	}
	if m2.State != "running" || m2.PID != 4242 {
		t.Fatalf("m2 = %+v, want state=running pid=4242", m2)
	}
}

func TestRunningMachinesAcrossApps(t *testing.T) {
	s := openTemp(t)
	for _, app := range []string{"a", "b"} {
		if err := s.UpsertApp(app, "{}"); err != nil {
			t.Fatalf("upsert app: %v", err)
		}
	}
	relA, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `[]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	relB, err := s.InsertRelease("b", "img:1", "/rootfs/b-1.ext4", `[]`, `{}`, "/")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	ipA, _ := s.AllocIP()
	if err := s.InsertMachine("ma", "a", relA, ipA, "tap-a"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	ipB, _ := s.AllocIP()
	if err := s.InsertMachine("mb", "b", relB, ipB, "tap-b"); err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	if err := s.SetMachineState("ma", "running", 1); err != nil {
		t.Fatalf("set state: %v", err)
	}
	// mb stays "starting"

	running, err := s.RunningMachines()
	if err != nil {
		t.Fatalf("running machines: %v", err)
	}
	if len(running) != 1 || running[0].ID != "ma" {
		t.Fatalf("running = %+v, want just ma", running)
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s.PutSecret("a", "DATABASE_URL", []byte("cipher-1")); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if err := s.PutSecret("a", "API_KEY", []byte("cipher-2")); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	// overwrite
	if err := s.PutSecret("a", "API_KEY", []byte("cipher-2b")); err != nil {
		t.Fatalf("put secret overwrite: %v", err)
	}

	secrets, err := s.Secrets("a")
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("len(secrets) = %d, want 2", len(secrets))
	}
	if string(secrets["DATABASE_URL"]) != "cipher-1" {
		t.Fatalf("DATABASE_URL = %q", secrets["DATABASE_URL"])
	}
	if string(secrets["API_KEY"]) != "cipher-2b" {
		t.Fatalf("API_KEY = %q", secrets["API_KEY"])
	}
}

func TestAppsListsRegisteredApps(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("b", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	apps, err := s.Apps()
	if err != nil {
		t.Fatalf("apps: %v", err)
	}
	if len(apps) != 2 || apps[0] != "a" || apps[1] != "b" {
		t.Fatalf("apps = %v, want [a b]", apps)
	}
}

func TestAppConfigRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", `{"app":"a"}`); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s.UpsertApp("a", `{"app":"a","image":"img:2"}`); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	config, err := s.AppConfig("a")
	if err != nil {
		t.Fatalf("app config: %v", err)
	}
	if config != `{"app":"a","image":"img:2"}` {
		t.Fatalf("config = %q, want the latest upsert", config)
	}
	if _, err := s.AppConfig("missing"); err == nil {
		t.Fatal("want error for unknown app")
	}
}

func TestReleaseByID(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	relID, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `["/bin/app"]`, `["FOO=1"]`, "/app")
	if err != nil {
		t.Fatalf("insert release: %v", err)
	}
	rel, err := s.ReleaseByID(relID)
	if err != nil {
		t.Fatalf("release by id: %v", err)
	}
	if rel.App != "a" || rel.Image != "img:1" || rel.RootFS != "/rootfs/a-1.ext4" || rel.Workdir != "/app" {
		t.Fatalf("release = %+v", rel)
	}
	if _, err := s.ReleaseByID(relID + 1); err == nil {
		t.Fatal("want error for unknown release id")
	}
}

func TestVolumesRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s.CreateVolume("a", "data", "/var/lib/oak/volumes/a-data.ext4", 10); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	volumes, err := s.Volumes("a")
	if err != nil {
		t.Fatalf("volumes: %v", err)
	}
	if len(volumes) != 1 {
		t.Fatalf("len(volumes) = %d, want 1", len(volumes))
	}
	if volumes[0].Name != "data" || volumes[0].SizeGB != 10 {
		t.Fatalf("volume = %+v", volumes[0])
	}
}

func TestLatestReleaseReturnsNewest(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if _, err := s.InsertRelease("a", "img:1", "/rootfs/a-1.ext4", `[]`, `[]`, "/"); err != nil {
		t.Fatalf("insert release 1: %v", err)
	}
	rel2ID, err := s.InsertRelease("a", "img:2", "/rootfs/a-2.ext4", `[]`, `[]`, "/")
	if err != nil {
		t.Fatalf("insert release 2: %v", err)
	}

	latest, err := s.LatestRelease("a")
	if err != nil {
		t.Fatalf("latest release: %v", err)
	}
	if latest.ID != rel2ID || latest.Image != "img:2" {
		t.Fatalf("latest release = %+v, want id %d image img:2", latest, rel2ID)
	}
}

func TestLatestReleaseErrorsWhenAppHasNone(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if _, err := s.LatestRelease("a"); err == nil {
		t.Fatal("want error for app with no releases")
	}
}

func TestDeleteSecretRemovesOnlyThatKey(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s.PutSecret("a", "FOO", []byte("cipher-foo")); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if err := s.PutSecret("a", "BAR", []byte("cipher-bar")); err != nil {
		t.Fatalf("put secret: %v", err)
	}

	if err := s.DeleteSecret("a", "FOO"); err != nil {
		t.Fatalf("delete secret: %v", err)
	}

	secrets, err := s.Secrets("a")
	if err != nil {
		t.Fatalf("secrets: %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("secrets = %+v, want just BAR", secrets)
	}
	if _, ok := secrets["BAR"]; !ok {
		t.Fatalf("BAR missing after deleting FOO: %+v", secrets)
	}
}

// TestDeletePerAppMethodsOnlyRemoveTargetAppsRows covers DeleteReleasesForApp,
// DeleteSecretsForApp and DeleteVolumesForApp: each must scope its DELETE to
// the given app and leave a second app's rows of the same kind untouched.
func TestDeletePerAppMethodsOnlyRemoveTargetAppsRows(t *testing.T) {
	s := openTemp(t)
	for _, app := range []string{"a", "b"} {
		if err := s.UpsertApp(app, "{}"); err != nil {
			t.Fatalf("upsert app %s: %v", app, err)
		}
		if _, err := s.InsertRelease(app, "img:1", "/rootfs/"+app+"-1.ext4", `[]`, `[]`, "/"); err != nil {
			t.Fatalf("insert release for %s: %v", app, err)
		}
		if err := s.PutSecret(app, "FOO", []byte("cipher")); err != nil {
			t.Fatalf("put secret for %s: %v", app, err)
		}
		if err := s.CreateVolume(app, "data", "/volumes/"+app+"-data.img", 5); err != nil {
			t.Fatalf("create volume for %s: %v", app, err)
		}
	}

	if err := s.DeleteReleasesForApp("a"); err != nil {
		t.Fatalf("delete releases for a: %v", err)
	}
	if err := s.DeleteSecretsForApp("a"); err != nil {
		t.Fatalf("delete secrets for a: %v", err)
	}
	if err := s.DeleteVolumesForApp("a"); err != nil {
		t.Fatalf("delete volumes for a: %v", err)
	}

	if _, err := s.LatestRelease("a"); err == nil {
		t.Fatal("want a's releases gone")
	}
	if secrets, err := s.Secrets("a"); err != nil || len(secrets) != 0 {
		t.Fatalf("a's secrets = %+v, err %v, want none", secrets, err)
	}
	if volumes, err := s.Volumes("a"); err != nil || len(volumes) != 0 {
		t.Fatalf("a's volumes = %+v, err %v, want none", volumes, err)
	}

	if _, err := s.LatestRelease("b"); err != nil {
		t.Fatalf("b's release should survive: %v", err)
	}
	if secrets, err := s.Secrets("b"); err != nil || len(secrets) != 1 {
		t.Fatalf("b's secrets = %+v, err %v, want 1", secrets, err)
	}
	if volumes, err := s.Volumes("b"); err != nil || len(volumes) != 1 {
		t.Fatalf("b's volumes = %+v, err %v, want 1", volumes, err)
	}
}

// TestFullCascadeDeleteLeavesNoOrphans exercises the exact sequence
// Deployer.Destroy runs (machines, then releases/secrets/volumes, then the
// app row) and checks nothing is left behind for the destroyed app while a
// second app is untouched.
func TestFullCascadeDeleteLeavesNoOrphans(t *testing.T) {
	s := openTemp(t)
	for _, app := range []string{"a", "b"} {
		if err := s.UpsertApp(app, "{}"); err != nil {
			t.Fatalf("upsert app %s: %v", app, err)
		}
		relID, err := s.InsertRelease(app, "img:1", "/rootfs/"+app+"-1.ext4", `[]`, `[]`, "/")
		if err != nil {
			t.Fatalf("insert release for %s: %v", app, err)
		}
		if _, err := s.AllocAndInsertMachine(app+"-m1", app, relID, "tap-"+app); err != nil {
			t.Fatalf("insert machine for %s: %v", app, err)
		}
		if err := s.SetMachineState(app+"-m1", "running", 111); err != nil {
			t.Fatalf("set machine state for %s: %v", app, err)
		}
		if err := s.PutSecret(app, "FOO", []byte("cipher")); err != nil {
			t.Fatalf("put secret for %s: %v", app, err)
		}
		if err := s.CreateVolume(app, "data", "/volumes/"+app+"-data.img", 5); err != nil {
			t.Fatalf("create volume for %s: %v", app, err)
		}
	}

	// Destroy "a": machines first (FK on apps.name), then the rest.
	if err := s.DeleteMachine("a-m1"); err != nil {
		t.Fatalf("delete machine: %v", err)
	}
	if err := s.DeleteReleasesForApp("a"); err != nil {
		t.Fatalf("delete releases: %v", err)
	}
	if err := s.DeleteSecretsForApp("a"); err != nil {
		t.Fatalf("delete secrets: %v", err)
	}
	if err := s.DeleteVolumesForApp("a"); err != nil {
		t.Fatalf("delete volumes: %v", err)
	}
	if err := s.DeleteApp("a"); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	apps, err := s.Apps()
	if err != nil {
		t.Fatalf("apps: %v", err)
	}
	if len(apps) != 1 || apps[0] != "b" {
		t.Fatalf("apps = %v, want just [b]", apps)
	}
	if machines, err := s.MachinesForApp("a"); err != nil || len(machines) != 0 {
		t.Fatalf("a's machines = %+v, err %v, want none", machines, err)
	}
	if _, err := s.LatestRelease("a"); err == nil {
		t.Fatal("want a's releases gone")
	}
	if secrets, err := s.Secrets("a"); err != nil || len(secrets) != 0 {
		t.Fatalf("a's secrets = %+v, err %v, want none", secrets, err)
	}
	if volumes, err := s.Volumes("a"); err != nil || len(volumes) != 0 {
		t.Fatalf("a's volumes = %+v, err %v, want none", volumes, err)
	}

	// b is fully intact.
	if machines, err := s.MachinesForApp("b"); err != nil || len(machines) != 1 {
		t.Fatalf("b's machines = %+v, err %v, want 1", machines, err)
	}
	if _, err := s.LatestRelease("b"); err != nil {
		t.Fatalf("b's release should survive: %v", err)
	}
	if secrets, err := s.Secrets("b"); err != nil || len(secrets) != 1 {
		t.Fatalf("b's secrets = %+v, err %v, want 1", secrets, err)
	}
	if volumes, err := s.Volumes("b"); err != nil || len(volumes) != 1 {
		t.Fatalf("b's volumes = %+v, err %v, want 1", volumes, err)
	}
}

// --- app ownership ---------------------------------------------------------

func TestSetAppOwnerSetsOnEmptyButNotOnAlreadyOwned(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}

	if owner, err := s.AppOwner("a"); err != nil || owner != "" {
		t.Fatalf("owner before SetAppOwner = %q, err %v, want \"\"", owner, err)
	}

	if err := s.SetAppOwner("a", "alice"); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	if owner, err := s.AppOwner("a"); err != nil || owner != "alice" {
		t.Fatalf("owner = %q, err %v, want alice", owner, err)
	}

	// A second call (e.g. a redeploy with a different owner) must not
	// clobber the owner recorded on first deploy.
	if err := s.SetAppOwner("a", "bob"); err != nil {
		t.Fatalf("set owner again: %v", err)
	}
	if owner, err := s.AppOwner("a"); err != nil || owner != "alice" {
		t.Fatalf("owner after second SetAppOwner = %q, err %v, want unchanged alice", owner, err)
	}
}

func TestAppsDetailedReturnsNamesAndOwnersSorted(t *testing.T) {
	s := openTemp(t)
	if err := s.UpsertApp("b", "{}"); err != nil {
		t.Fatalf("upsert app b: %v", err)
	}
	if err := s.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app a: %v", err)
	}
	if err := s.SetAppOwner("a", "alice"); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	apps, err := s.AppsDetailed()
	if err != nil {
		t.Fatalf("apps detailed: %v", err)
	}
	want := []AppInfo{{Name: "a", Owner: "alice"}, {Name: "b", Owner: ""}}
	if len(apps) != len(want) || apps[0] != want[0] || apps[1] != want[1] {
		t.Fatalf("apps detailed = %+v, want %+v", apps, want)
	}
}

// TestAddAppsOwnerColumnMigratesPreExistingSchema exercises the migration
// oakd's Open runs against a database created before apps.owner existed:
// schemaSQL's CREATE TABLE IF NOT EXISTS is a no-op against such a
// database, so addAppsOwnerColumn must ALTER TABLE it in -- and do so
// without error if called again (Open runs it on every start).
func TestAddAppsOwnerColumnMigratesPreExistingSchema(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The pre-owner-column apps table shape.
	if _, err := db.Exec(`CREATE TABLE apps (
		name TEXT PRIMARY KEY, node_id TEXT NOT NULL DEFAULT 'local',
		config TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("create old apps table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO apps(name, config) VALUES ('a', '{}')`); err != nil {
		t.Fatalf("insert app row: %v", err)
	}

	if err := addAppsOwnerColumn(db); err != nil {
		t.Fatalf("migrate (1st): %v", err)
	}
	var owner string
	if err := db.QueryRow(`SELECT owner FROM apps WHERE name = 'a'`).Scan(&owner); err != nil {
		t.Fatalf("select owner after migration: %v", err)
	}
	if owner != "" {
		t.Fatalf("owner = %q, want \"\" default", owner)
	}

	// Idempotent: a database that already has the column must not error.
	if err := addAppsOwnerColumn(db); err != nil {
		t.Fatalf("migrate (2nd, idempotent): %v", err)
	}
}

// TestOpenTwiceIsIdempotent covers the acceptance criterion literally:
// opening the same on-disk store twice (the schema, including the owner
// migration, applies on every Open) must not error the second time.
func TestOpenTwiceIsIdempotent(t *testing.T) {
	path := t.TempDir() + "/oakd.db"

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open (1st): %v", err)
	}
	if err := s1.UpsertApp("a", "{}"); err != nil {
		t.Fatalf("upsert app: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("open (2nd): %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if owner, err := s2.AppOwner("a"); err != nil || owner != "" {
		t.Fatalf("owner after reopen = %q, err %v, want \"\"", owner, err)
	}
}

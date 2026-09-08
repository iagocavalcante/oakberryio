package store

import "testing"

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

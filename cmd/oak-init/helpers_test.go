package main

import "testing"

func TestResolvConf(t *testing.T) {
	got := resolvConf("10.200.0.1")
	want := "nameserver 10.200.0.1\n"
	if got != want {
		t.Errorf("resolvConf = %q, want %q", got, want)
	}
}

func TestHostsFile(t *testing.T) {
	got := hostsFile("m-abc123")
	want := "127.0.0.1\tlocalhost\n127.0.0.1\tm-abc123\n"
	if got != want {
		t.Errorf("hostsFile = %q, want %q", got, want)
	}
}

func TestBaselineEnv(t *testing.T) {
	got := baselineEnv(nil)
	if got[0] != "PATH="+defaultPath || got[1] != "HOME=/root" {
		t.Errorf("baselineEnv(nil) = %q", got)
	}
	got = baselineEnv(map[string]string{"PATH": "/app/bin", "HOME": "/home/app"})
	if got[0] != "PATH=/app/bin" || got[1] != "HOME=/home/app" {
		t.Errorf("baselineEnv(image env) = %q, want image values kept", got)
	}
}

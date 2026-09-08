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

package vm

import "testing"

func TestMACFromIP(t *testing.T) {
	mac, err := MACFromIP("10.200.0.5")
	if err != nil {
		t.Fatalf("MACFromIP: %v", err)
	}
	if mac != "06:00:0a:c8:00:05" {
		t.Errorf("MACFromIP(10.200.0.5) = %q, want 06:00:0a:c8:00:05", mac)
	}
}

func TestMACFromIPRejectsNonIPv4(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "::1", ""} {
		if _, err := MACFromIP(bad); err == nil {
			t.Errorf("MACFromIP(%q): want error", bad)
		}
	}
}

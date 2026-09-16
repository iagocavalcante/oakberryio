package cli

import (
	"errors"
	"testing"
)

func TestStreamOutcome(t *testing.T) {
	cases := []struct {
		last    string
		readErr error
		wantErr bool
	}{
		{"ok abc123", nil, false},
		{"ok abc123", errors.New("stream reset"), false}, // tunnel restarted after cutover
		{"error: health check never passed", nil, true},
		{"waiting for health check...", errors.New("stream reset"), true},
		{"tunnel restart failed: x; deploy is live", nil, false},
	}
	for _, c := range cases {
		err := streamOutcome("deploy", c.last, c.readErr)
		if (err != nil) != c.wantErr {
			t.Errorf("streamOutcome(%q, %v) = %v, wantErr=%v", c.last, c.readErr, err, c.wantErr)
		}
	}
}

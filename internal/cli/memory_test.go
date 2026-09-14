package cli

import "testing"

func TestParseMemoryMB(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "1gb", want: 1024},
		{in: "2GB", want: 2048},
		{in: "512mb", want: 512},
		{in: "1024", want: 1024},
		{in: "nonsense", wantErr: true},
		{in: "", wantErr: true},
		{in: "-512mb", wantErr: true},
		{in: "-1", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseMemoryMB(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMemoryMB(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemoryMB(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMemoryMB(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

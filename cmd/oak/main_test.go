package main

import (
	"flag"
	"reflect"
	"testing"
)

// TestParseFlagsAnywhere covers the property the bare flag package lacks:
// flags are honored whether they come before or after the positional args.
func TestParseFlagsAnywhere(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantPos  []string
		wantBool bool
		wantStr  string
	}{
		{"flag after positional", []string{"app", "-y"}, []string{"app"}, true, ""},
		{"flag before positional", []string{"-y", "app"}, []string{"app"}, true, ""},
		{"no flag", []string{"app"}, []string{"app"}, false, ""},
		{"value flag after positional", []string{"app", "-s", "1gb"}, []string{"app"}, false, "1gb"},
		{"value flag before positional", []string{"-s", "1gb", "app"}, []string{"app"}, false, "1gb"},
		{"interspersed", []string{"-y", "app", "-s", "512mb"}, []string{"app"}, true, "512mb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			b := fs.Bool("y", false, "")
			s := fs.String("s", "", "")
			pos, err := parseFlagsAnywhere(fs, tc.args)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positional = %v, want %v", pos, tc.wantPos)
			}
			if *b != tc.wantBool {
				t.Errorf("-y = %v, want %v", *b, tc.wantBool)
			}
			if *s != tc.wantStr {
				t.Errorf("-s = %q, want %q", *s, tc.wantStr)
			}
		})
	}
}

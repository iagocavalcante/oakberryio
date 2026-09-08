package cli

import (
	"reflect"
	"testing"
)

func TestParseKV(t *testing.T) {
	got, err := ParseKV([]string{"A=1", "B=2"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestParseKVAllowsEqualsInValue(t *testing.T) {
	got, err := ParseKV([]string{"URL=https://a=b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["URL"] != "https://a=b" {
		t.Fatalf("got %q", got["URL"])
	}
}

func TestParseKVRejectsMissingEquals(t *testing.T) {
	if _, err := ParseKV([]string{"NOEQUALS"}); err == nil {
		t.Fatal("want error")
	}
}

func TestParseKVRejectsEmptyKey(t *testing.T) {
	if _, err := ParseKV([]string{"=value"}); err == nil {
		t.Fatal("want error")
	}
}

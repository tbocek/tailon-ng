package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestParseTerms runs the cases shared with the frontend's parseTerms.
func TestParseTerms(t *testing.T) {
	data, err := os.ReadFile("testdata/terms.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Q      string
		Groups [][]string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := parseTerms(c.Q); len(got)+len(c.Groups) > 0 && !reflect.DeepEqual(got, c.Groups) {
			t.Errorf("parseTerms(%q) = %q, want %q", c.Q, got, c.Groups)
		}
	}
}

func TestTermMatch(t *testing.T) {
	for _, c := range []struct {
		q, line string
		nocase  bool
		want    bool
	}{
		{"error warn", "a warn here", false, true},
		{"error warn", "all fine", false, false},
		{"error && nginx", "nginx: error 502", false, true},
		{"error && nginx", "error in sshd", false, false},
		{"error warn && nginx", "warn from nginx", false, true},
		{"error warn && nginx", "warn from sshd", false, false},
		{`"connection refused"`, "refused connection", false, false},
		{"ERROR", "error here", false, false},
		{"Error && NGINX", "nginx error", true, true},
	} {
		if got := termMatch(parseTerms(c.q), c.nocase)(c.line); got != c.want {
			t.Errorf("%q (nocase %v) on %q = %v, want %v", c.q, c.nocase, c.line, got, c.want)
		}
	}
}

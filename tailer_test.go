package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLastLinesLineEndings checks LF, CRLF and CR-only files all split into
// lines — a CR-only tail window used to come back empty, rendering as a blank
// view, and CRLF lines carried an invisible trailing \r.
func TestLastLinesLineEndings(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		data string
		want []string
	}{
		{"lf.log", "a\nb\nc\n", []string{"b", "c"}},
		{"crlf.log", "a\r\nb\r\nc\r\n", []string{"b", "c"}},
		{"cr.log", "a\rb\rc\r", []string{"b", "c"}},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name)
		if err := os.WriteFile(p, []byte(c.data), 0o644); err != nil {
			t.Fatal(err)
		}
		got, _ := lastLines(p, 2)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestDetectLayout(t *testing.T) {
	// A file consistently in ISO "space" format (with a continuation line that
	// has no timestamp). The format is chosen across the lines, not from one.
	iso := []string{
		"2026-06-29 17:48:12 started",
		"2026-06-29 17:48:13 working",
		"    ... continuation line, no timestamp",
		"2026-06-29 17:48:14 done",
	}
	if got := detectLayout(iso); got != "2006-01-02 15:04:05.999999999" {
		t.Errorf("ISO: detected %q", got)
	}

	// Syslog (RFC 3164).
	syslog := []string{
		"Jun 29 17:48:12 host app: up",
		"Jun 29 17:48:13 host app: ok",
	}
	if got := detectLayout(syslog); got != "Jan 2 15:04:05" {
		t.Errorf("syslog: detected %q", got)
	}

	// No timestamps at all.
	if got := detectLayout([]string{"hello", "world"}); got != "" {
		t.Errorf("none: detected %q", got)
	}
}

func TestMatchAny(t *testing.T) {
	cases := []struct {
		line  string
		match bool
	}{
		{"2026-06-29T17:48:12+02:00 hello", true},       // RFC3339 + trailing text
		{"2026-06-29T17:48:12 hello", true},             // ISO 8601, no timezone
		{"2026-06-29 17:48:12.123 hello", true},         // fractional seconds
		{"<13>2026-06-29 17:48:12 msg", true},           // syslog priority skipped
		{"[29/Jun/2026:17:48:12 +0200] GET /", true},    // Apache/CLF in brackets
		{"Jun 29 17:48:12 host app: up", true},          // syslog RFC 3164
		{"Mon Jun 29 17:48:12 2026 boot", true},         // Unix ctime
		{"just a plain log line without a date", false}, // no timestamp
	}
	for _, c := range cases {
		if _, ok := matchAny(c.line); ok != c.match {
			t.Errorf("matchAny(%q) = %v, want %v", c.line, ok, c.match)
		}
	}
}

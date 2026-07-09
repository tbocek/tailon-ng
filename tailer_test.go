package main

import (
	"fmt"
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

// TestLastLinesWindowScales checks the backlog read window grows with n: a
// view asking for the full scrollback must reach past the old 256KB cap.
func TestLastLinesWindowScales(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	const total = 5000 // ~600KB, comfortably past 256KB
	for i := 0; i < total; i++ {
		fmt.Fprintf(f, "line %05d with some padding to reach realistic length %060d\n", i, i)
	}
	f.Close()

	if lines, _ := lastLines(p, 50000); len(lines) != total {
		t.Fatalf("want the whole file (%d lines) within the scaled window, got %d", total, len(lines))
	}
	if lines, _ := lastLines(p, 10); len(lines) != 10 {
		t.Fatalf("small n: want 10 lines, got %d", len(lines))
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

// TestPrimeTimestamper checks a stamper primed from the file: the layout comes
// from the trailing lines, and the "previous date" undated backlog lines
// inherit is the last one before the backlog window — found by looking back in
// the log, so a backlog that starts mid-entry (a stack trace, say) sorts with
// its entry instead of as "now".
func TestPrimeTimestamper(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "p.log")
	data := "2026-06-29 17:00:00 started\n" +
		"2026-06-29 17:05:00 crashed\n" +
		"  at frame one\n" +
		"  at frame two\n" +
		"  at frame three\n"
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	// The backlog is the last 3 lines — all undated continuations.
	ts := primeTimestamper(p, 3)
	if ts.layout != "2006-01-02 15:04:05.999999999" {
		t.Fatalf("layout = %q", ts.layout)
	}
	want, _ := matchAny("2026-06-29 17:05:00")
	if !ts.last.Equal(want) {
		t.Fatalf("last = %v, want %v", ts.last, want)
	}
	if got := ts.stamp("  at frame one"); !got.Equal(want) {
		t.Fatalf("undated backlog line stamped %v, want the primed %v", got, want)
	}

	// A file with no timestamps at all locks "none": stamping falls back to
	// time.Now — ordering by arrival, never displayed.
	np := filepath.Join(dir, "n.log")
	if err := os.WriteFile(np, []byte("plain\nlines\nonly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ts := primeTimestamper(np, 2); ts.layout != "none" || !ts.last.IsZero() {
		t.Fatalf("undated file: layout=%q last=%v", ts.layout, ts.last)
	}

	// A backlog window larger than the file: nothing precedes it, no last.
	if ts := primeTimestamper(p, 99); !ts.last.IsZero() {
		t.Fatalf("skip beyond file: last = %v, want zero", ts.last)
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

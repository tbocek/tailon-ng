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
	for i := range total {
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

func TestStamp(t *testing.T) {
	ts := &timestamper{}

	// The first dated line picks the layout.
	first := ts.stamp("2026-06-29 17:48:12 started")
	if ts.layout != "2006-01-02 15:04:05.999999999" {
		t.Fatalf("layout = %q", ts.layout)
	}

	// An undated continuation line inherits the previous timestamp.
	if got := ts.stamp("    ... continuation line, no timestamp"); !got.Equal(first) {
		t.Fatalf("continuation stamped %v, want %v", got, first)
	}

	// Nothing is locked in: a line in another format switches the layout.
	ts.stamp("Jun 29 17:48:13 host app: ok")
	if ts.layout != "Jan 2 15:04:05" {
		t.Fatalf("layout after syslog line = %q", ts.layout)
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
	want, _ := matchLayout(ts.layout, "2026-06-29 17:05:00")
	if !ts.last.Equal(want) {
		t.Fatalf("last = %v, want %v", ts.last, want)
	}
	if got := ts.stamp("  at frame one"); !got.Equal(want) {
		t.Fatalf("undated backlog line stamped %v, want the primed %v", got, want)
	}

	// A file with no timestamps at all is anchored at its modification time,
	// so its lines sort near the file's true age instead of as "now".
	np := filepath.Join(dir, "n.log")
	if err := os.WriteFile(np, []byte("plain\nlines\nonly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(np)
	if err != nil {
		t.Fatal(err)
	}
	if ts := primeTimestamper(np, 2); ts.layout != "" || !ts.last.Equal(fi.ModTime()) {
		t.Fatalf("undated file: layout=%q last=%v, want mtime %v", ts.layout, ts.last, fi.ModTime())
	}

	// A backlog window larger than the file: nothing precedes the backlog, so
	// the anchor falls back to the file's modification time here too.
	pfi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if ts := primeTimestamper(p, 99); !ts.last.Equal(pfi.ModTime()) {
		t.Fatalf("skip beyond file: last = %v, want mtime %v", ts.last, pfi.ModTime())
	}
}

func TestStampLayouts(t *testing.T) {
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
		ts := &timestamper{}
		ts.stamp(c.line)
		if matched := ts.layout != ""; matched != c.match {
			t.Errorf("stamp(%q) matched = %v, want %v", c.line, matched, c.match)
		}
	}
}

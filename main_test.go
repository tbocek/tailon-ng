package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIsStale(t *testing.T) {
	cases := []struct {
		path  string
		stale bool
	}{
		{"/var/log/syslog", false},
		{"/var/log/remote/OpenWrt.log", false},
		{"/var/log/remote/192.168.1.5.log-20260702.gz", true}, // logrotate dateext + compress
		{"/var/log/messages.1", true},                         // numeric rotation
		{"/var/log/messages.2.gz", true},
		{"/var/log/app.log-20260702", true}, // dateext before compression (delaycompress)
		{"/var/log/app.log.20260702", true},
		{"/var/log/app.log.bz2", true},
		{"/var/log/app.log.xz", true},
		{"/var/log/app.log.zst", true},
		{"/var/log/app.log.old", true},
		{"/var/log/app.log.bak", true},
	}
	for _, c := range cases {
		if got := isStale(c.path); got != c.stale {
			t.Errorf("isStale(%q) = %v, want %v", c.path, got, c.stale)
		}
	}
}

func paths(entries []*ListEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

func TestListingFiles(t *testing.T) {
	lst := createListing([]string{"testdata/var/log/2.log", "testdata/var/log/1.log"})
	// Listing is path-sorted, so 1.log comes before 2.log regardless of order.
	want := []string{"testdata/var/log/1.log", "testdata/var/log/2.log"}
	if got := paths(lst); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if lst[0].Stale {
		t.Fatal("expected 1.log to be live, not stale")
	}
}

func TestListingDir(t *testing.T) {
	lst := createListing([]string{"testdata/var/log/"})
	if len(lst) != 5 {
		t.Fatalf("expected 5 files served recursively, got %d: %q", len(lst), paths(lst))
	}
	if !fileAllowed("testdata/var/log/1.log") {
		t.Fatal("expected 1.log to be allowed after the directory walk")
	}
	// The rotated archive is listed, but as stale: excluded from live tailing.
	if lst[1].Path != "testdata/var/log/1.log.1.gz" || !lst[1].Stale {
		t.Fatalf("expected 1.log.1.gz to be listed stale, got %+v", lst[1])
	}
}

func TestListingSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.log"), []byte("2026-07-18 ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wtmp"), []byte("\x00\x01\x02binary\x00junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	lst := createListing([]string{dir})
	if len(lst) != 1 || filepath.Base(lst[0].Path) != "app.log" {
		t.Fatalf("expected only app.log served, got %q", paths(lst))
	}
	if fileAllowed(filepath.Join(dir, "wtmp")) {
		t.Fatal("binary file must not be in the allowlist")
	}
}

func TestListingGlob(t *testing.T) {
	lst := createListing([]string{"testdata/var/log/*.log"})
	if len(lst) != 4 {
		t.Fatalf("glob expanded to %d files, want 4: %q", len(lst), paths(lst))
	}
}

func TestListingDoubleStar(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"a.log", "sub/b.log", "sub/deep/c.log", "sub/note.txt"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		pattern string
		want    int
	}{
		{"**.log", 3},   // every .log at any depth
		{"**/*.log", 3}, // same set: "**/" may span zero directories
		{"*.log", 1},    // single level only (single-level glob, not "**")
		{"sub/**.log", 2},
	}
	for _, c := range cases {
		got := len(createListing([]string{filepath.Join(dir, c.pattern)}))
		if got != c.want {
			t.Errorf("%q matched %d files, want %d", c.pattern, got, c.want)
		}
	}
}

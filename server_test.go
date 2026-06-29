package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if len(c.BindAddr) != 1 || c.BindAddr[0] != ":8080" {
		t.Errorf("BindAddr = %v", c.BindAddr)
	}
	if c.RelativeRoot != "/" {
		t.Errorf("RelativeRoot = %q", c.RelativeRoot)
	}
	if !c.AllowDownload {
		t.Error("AllowDownload should default to true")
	}
}

// setupConfig points the global config at a filespec and builds the
// allowed-file listing, the way main() does at startup.
func setupConfig(t *testing.T, spec string) {
	t.Helper()
	config = defaultConfig()
	fs, err := parseFileSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	config.FileSpecs = []FileSpec{fs}
	createListing(config.FileSpecs)
}

func TestListHandler(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")

	w := httptest.NewRecorder()
	listHandler(w, httptest.NewRequest("GET", "/list", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var listing map[string][]*ListEntry
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	entries := listing["__default__"]
	if len(entries) != 1 || entries[0].Path != "testdata/ex1/var/log/1.log" {
		t.Fatalf("unexpected listing: %#v", listing)
	}
}

// TestListingNoRace hammers createListing (which rebuilds the global allFiles)
// against the concurrent readers fileAllowed/allowedFiles, as happens when a
// /list request overlaps a /stream or /files request. It must pass under -race.
func TestListingNoRace(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/*.log")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); createListing(config.FileSpecs) }()
		go func() {
			defer wg.Done()
			_ = allowedFiles()
			fileAllowed("testdata/ex1/var/log/1.log")
		}()
	}
	wg.Wait()
}

// readSSEData reads up to n SSE "data:" payloads from r, failing after timeout.
func readSSEData(t *testing.T, r io.Reader, n int, timeout time.Duration) []string {
	t.Helper()
	out := make(chan []string, 1)
	go func() {
		var lines []string
		scanner := bufio.NewScanner(r)
		for len(lines) < n && scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				lines = append(lines, data)
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("reading SSE stream: %v", err)
		}
		out <- lines
	}()
	select {
	case lines := <-out:
		return lines
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d SSE frames", n)
		return nil
	}
}

// TestStreamTail checks tail mode shows the last nlines lines.
func TestStreamTail(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&path=testdata/ex1/var/log/1.log&nlines=2")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	got := readSSEData(t, resp.Body, 2, 5*time.Second)
	if want := []string{`"2"`, `"3"`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestStreamGrep checks grep mode reads the whole file from the start.
func TestStreamGrep(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=grep&path=testdata/ex1/var/log/1.log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := readSSEData(t, resp.Body, 3, 5*time.Second)
	if want := []string{`"1"`, `"2"`, `"3"`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestStreamFilter checks the regexp filter and its inverse (done in Go).
func TestStreamFilter(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()
	p := "path=testdata/ex1/var/log/1.log"

	// Only lines matching "2".
	resp, err := http.Get(srv.URL + "?mode=grep&" + p + "&filter=2")
	if err != nil {
		t.Fatal(err)
	}
	got := readSSEData(t, resp.Body, 1, 5*time.Second)
	resp.Body.Close()
	if want := []string{`"2"`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filter: got %v, want %v", got, want)
	}

	// Inverted: lines NOT matching "2".
	resp, err = http.Get(srv.URL + "?mode=grep&" + p + "&filter=2&invert=1")
	if err != nil {
		t.Fatal(err)
	}
	got = readSSEData(t, resp.Body, 2, 5*time.Second)
	resp.Body.Close()
	if want := []string{`"1"`, `"3"`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("invert: got %v, want %v", got, want)
	}
}

// TestStreamFollow checks that tail mode streams lines appended after connect.
func TestStreamFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config = defaultConfig()
	fs, _ := parseFileSpec(path)
	config.FileSpecs = []FileSpec{fs}
	createListing(config.FileSpecs)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&nlines=2&path=" + url.QueryEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	readData := func() string {
		for scanner.Scan() {
			if d, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				return d
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("reading SSE stream: %v", err)
		}
		return ""
	}
	if got := readData(); got != `"a"` {
		t.Fatalf("first line: %q", got)
	}
	if got := readData(); got != `"b"` {
		t.Fatalf("second line: %q", got)
	}

	// Append a line; it must be followed.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("c\n")
	f.Close()

	done := make(chan string, 1)
	go func() { done <- readData() }()
	select {
	case got := <-done:
		if got != `"c"` {
			t.Fatalf("followed line: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the appended line")
	}
}

// TestStreamAllFiles checks the all=1 mode streams every file, prefixed by path.
func TestStreamAllFiles(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/*.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&all=1&nlines=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := readSSEData(t, resp.Body, 2, 5*time.Second)
	found := false
	for _, frame := range got {
		var s string
		if json.Unmarshal([]byte(frame), &s) == nil && strings.Contains(s, "testdata/ex1/var/log/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected path-prefixed lines, got %v", got)
	}
}

// TestStreamAllFilesSorted checks all-files mode merges files in timestamp
// order rather than file order.
func TestStreamAllFilesSorted(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	if err := os.WriteFile(a, []byte("2026-01-01 00:00:01 a1\n2026-01-01 00:00:03 a3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("2026-01-01 00:00:02 b2\n2026-01-01 00:00:04 b4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config = defaultConfig()
	sa, _ := parseFileSpec(a)
	sb, _ := parseFileSpec(b)
	config.FileSpecs = []FileSpec{sa, sb}
	createListing(config.FileSpecs)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=grep&all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := readSSEData(t, resp.Body, 4, 5*time.Second)
	var markers []string
	for _, frame := range got {
		var s string
		if json.Unmarshal([]byte(frame), &s) == nil {
			f := strings.Fields(s)
			markers = append(markers, f[len(f)-1])
		}
	}
	if want := []string{"a1", "b2", "a3", "b4"}; !reflect.DeepEqual(markers, want) {
		t.Fatalf("merge order: got %v, want %v (frames %v)", markers, want, got)
	}
}

func TestStreamRejectsUnknownFile(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	w := httptest.NewRecorder()
	streamHandler(w, httptest.NewRequest("GET", "/stream?mode=tail&path=/etc/passwd", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a disallowed file, got %d", w.Code)
	}
}

func TestStreamRejectsBadFilter(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	w := httptest.NewRecorder()
	streamHandler(w, httptest.NewRequest("GET", "/stream?mode=grep&path=testdata/ex1/var/log/1.log&filter=%28", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid regexp, got %d", w.Code)
	}
}

// TestServerStack drives requests through the real router + access-log
// middleware (setupRoutes wrapped in loggingHandler).
func TestServerStack(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	ts := httptest.NewServer(loggingHandler(io.Discard, setupRoutes(config.RelativeRoot)))
	defer ts.Close()

	cases := []struct{ path, want string }{
		{"/", `id="toolbar"`},
		{"/vfs/dist/main.js", "EventSource"},
		{"/vfs/dist/main.css", "log-entry"},
		{"/list", "testdata/ex1/var/log/1.log"},
	}
	for _, tc := range cases {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s: body missing %q", tc.path, tc.want)
		}
	}
}

func TestIndexHandler(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")

	w := httptest.NewRecorder()
	indexHandler(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="toolbar"`) || !strings.Contains(body, "vfs/dist/main.js") {
		t.Fatal("index template did not render the expected content")
	}
}

func TestDownloadHandler(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")

	// Allowed file.
	w := httptest.NewRecorder()
	downloadHandler(w, httptest.NewRequest("GET", "/files/?path=testdata/ex1/var/log/1.log", nil))
	if w.Code != http.StatusOK {
		t.Errorf("allowed download: status = %d", w.Code)
	}

	// Missing path must not panic; it is simply unknown.
	w = httptest.NewRecorder()
	downloadHandler(w, httptest.NewRequest("GET", "/files/", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("missing path: status = %d", w.Code)
	}

	// Disallowed file.
	w = httptest.NewRecorder()
	downloadHandler(w, httptest.NewRequest("GET", "/files/?path=/etc/passwd", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("disallowed file: status = %d", w.Code)
	}

	// Downloads disabled entirely.
	config.AllowDownload = false
	w = httptest.NewRecorder()
	downloadHandler(w, httptest.NewRequest("GET", "/files/?path=testdata/ex1/var/log/1.log", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("downloads disabled: status = %d", w.Code)
	}
}

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
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

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func TestDefaultConfig(t *testing.T) {
	c := defaultConfig()
	if c.BindAddr != ":8080" {
		t.Errorf("BindAddr = %v", c.BindAddr)
	}
	if c.RelativeRoot != "/" {
		t.Errorf("RelativeRoot = %q", c.RelativeRoot)
	}
}

// setupConfig points the global config at a single source and builds the
// allowed-file listing, the way main() does at startup.
func setupConfig(t *testing.T, source string) {
	t.Helper()
	config = defaultConfig()
	config.Sources = []string{source}
	createListing(config.Sources)
}

func TestListHandler(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")

	w := httptest.NewRecorder()
	listHandler(w, httptest.NewRequest("GET", "/list", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var listing []*ListEntry
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(listing) != 1 || listing[0].Path != "testdata/ex1/var/log/1.log" {
		t.Fatalf("unexpected listing: %#v", listing)
	}
}

// TestListingNoRace hammers createListing (which rebuilds the global allFiles)
// against the concurrent readers fileAllowed/allowedFiles, as happens when a
// /list request overlaps a /stream or /files request. It must pass under -race.
func TestListingNoRace(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); createListing(config.Sources) }()
		go func() {
			defer wg.Done()
			_ = allowedFiles()
			fileAllowed("testdata/ex1/var/log/1.log")
		}()
	}
	wg.Wait()
}

// sseFrame mirrors the JSON object sent per line: T is the text, P the source
// file (multi-file streams), O the resume offset (single-file streams).
type sseFrame struct {
	P string `json:"p"`
	T string `json:"t"`
	O int64  `json:"o"`
}

// readSSEData reads up to n SSE "data:" frames from r, failing after timeout.
func readSSEData(t *testing.T, r io.Reader, n int, timeout time.Duration) []sseFrame {
	t.Helper()
	out := make(chan []sseFrame, 1)
	go func() {
		var frames []sseFrame
		scanner := bufio.NewScanner(r)
		event := "" // pending named event; its data is a control frame, not a line
		for len(frames) < n && scanner.Scan() {
			if name, ok := strings.CutPrefix(scanner.Text(), "event: "); ok {
				event = name
				continue
			}
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok && data != "" && event == "" {
				var f sseFrame
				if err := json.Unmarshal([]byte(data), &f); err != nil {
					t.Errorf("bad SSE frame %q: %v", data, err)
				}
				frames = append(frames, f)
			} else if ok {
				event = "" // consumed the control frame's data
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("reading SSE stream: %v", err)
		}
		out <- frames
	}()
	select {
	case frames := <-out:
		return frames
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %d SSE frames", n)
		return nil
	}
}

func frameTexts(frames []sseFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.T
	}
	return out
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

	got := frameTexts(readSSEData(t, resp.Body, 2, 5*time.Second))
	if want := []string{"2", "3"}; !reflect.DeepEqual(got, want) {
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

	got := frameTexts(readSSEData(t, resp.Body, 3, 5*time.Second))
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestStreamViewCap checks that view mode with nlines sends only the last
// nlines matching lines: the client discards anything past its scrollback, so
// the server does not ship it. Reading the whole file still drives progress.
func TestStreamViewCap(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log") // lines: 1, 2, 3
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=grep&nlines=2&path=testdata/ex1/var/log/1.log")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // view closes at EOF
	resp.Body.Close()
	got := frameTexts(readSSEData(t, strings.NewReader(string(body)), 99, 5*time.Second))
	if want := []string{"2", "3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("capped view: got %v, want %v", got, want)
	}
	if !strings.Contains(string(body), "event: progress") {
		t.Fatal("capped view should still report progress")
	}
}

// TestStreamFilter checks the regexp filter (done in Go).
func TestStreamFilter(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	// Only lines matching "2".
	resp, err := http.Get(srv.URL + "?mode=grep&path=testdata/ex1/var/log/1.log&filter=2")
	if err != nil {
		t.Fatal(err)
	}
	got := frameTexts(readSSEData(t, resp.Body, 1, 5*time.Second))
	resp.Body.Close()
	if want := []string{"2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("filter: got %v, want %v", got, want)
	}
}

// TestStreamFollow checks that tail mode streams lines appended after connect.
func TestStreamFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "follow.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config = defaultConfig()
	config.Sources = []string{path}
	createListing(config.Sources)

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
			if d, ok := strings.CutPrefix(scanner.Text(), "data: "); ok && d != "" {
				var f sseFrame
				json.Unmarshal([]byte(d), &f)
				return f.T
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("reading SSE stream: %v", err)
		}
		return ""
	}
	if got := readData(); got != "a" {
		t.Fatalf("first line: %q", got)
	}
	if got := readData(); got != "b" {
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
		if got != "c" {
			t.Fatalf("followed line: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the appended line")
	}
}

// TestStreamAllFiles checks the all=1 mode streams every file, prefixed by path.
func TestStreamAllFiles(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/")
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
		if strings.Contains(frame.P, "testdata/ex1/var/log/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected frames carrying their source path, got %v", got)
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
	config.Sources = []string{a, b}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&nlines=2&all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := readSSEData(t, resp.Body, 4, 5*time.Second)
	var markers []string
	for _, frame := range got {
		f := strings.Fields(frame.T)
		markers = append(markers, f[len(f)-1])
	}
	if want := []string{"a1", "b2", "a3", "b4"}; !reflect.DeepEqual(markers, want) {
		t.Fatalf("merge order: got %v, want %v (frames %v)", markers, want, got)
	}
}

// TestStreamScopedSubfolder checks all=1&scope=<dir> streams only the files
// under that subfolder, never a sibling's.
func TestStreamScopedSubfolder(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"host1", "host2"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "host1", "a.log"), []byte("aaa\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "host1", "c.log"), []byte("ccc\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "host2", "b.log"), []byte("bbb\n"), 0o644)

	config = defaultConfig()
	config.Sources = []string{dir}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	scope := filepath.Join(dir, "host1") + "/" // the client sends directory scopes with a trailing slash
	resp, err := http.Get(srv.URL + "?mode=tail&nlines=1&all=1&scope=" + url.QueryEscape(scope))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Two files under host1, so frames carry their path; none may be from host2.
	got := readSSEData(t, resp.Body, 2, 5*time.Second)
	for _, frame := range got {
		if !strings.Contains(frame.P, "host1") || strings.Contains(frame.P, "host2") {
			t.Fatalf("scope leaked outside host1: %+v", frame)
		}
	}
}

// writeArchives fills a directory with one live log and gz/xz/zst archives,
// then points the global config at it.
func writeArchives(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "live.log"), []byte("live1\nlive2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("from-gz\n"))
	zw.Close()
	os.WriteFile(filepath.Join(dir, "old.log.gz"), gz.Bytes(), 0o644)

	var xzBuf bytes.Buffer
	xw, err := xz.NewWriter(&xzBuf)
	if err != nil {
		t.Fatal(err)
	}
	xw.Write([]byte("from-xz\n"))
	xw.Close()
	os.WriteFile(filepath.Join(dir, "old.log.xz"), xzBuf.Bytes(), 0o644)

	var zstBuf bytes.Buffer
	sw, err := zstd.NewWriter(&zstBuf)
	if err != nil {
		t.Fatal(err)
	}
	sw.Write([]byte("from-zst\n"))
	sw.Close()
	os.WriteFile(filepath.Join(dir, "old.log.zst"), zstBuf.Bytes(), 0o644)

	config = defaultConfig()
	config.Sources = []string{dir}
	createListing(config.Sources)
	return dir
}

// TestAggregateTailSkipsArchives checks that an aggregate stream tails live
// files only: rotated/compressed leftovers never pollute it.
func TestAggregateTailSkipsArchives(t *testing.T) {
	writeArchives(t)
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&nlines=5&all=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := frameTexts(readSSEData(t, resp.Body, 2, 5*time.Second))
	if want := []string{"live1", "live2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tail(all): got %v, want %v", got, want)
	}
}

// TestStreamRejectsAggregateView checks that view (grep) is limited to single
// files: an aggregate view — and the removed grep-all mode — are rejected.
func TestStreamRejectsAggregateView(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/")
	for _, q := range []string{"mode=grep&all=1", "mode=grep&all=1&scope=testdata/ex1/var/", "mode=grep-all&all=1"} {
		w := httptest.NewRecorder()
		streamHandler(w, httptest.NewRequest("GET", "/stream?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
}

// TestStaleForcedToGrep checks that tail on an archive is coerced into a single
// decoded grep pass instead of following compressed bytes.
func TestStaleForcedToGrep(t *testing.T) {
	dir := writeArchives(t)
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&nlines=5&path=" + url.QueryEscape(filepath.Join(dir, "old.log.gz")))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := frameTexts(readSSEData(t, resp.Body, 1, 5*time.Second))
	if want := []string{"from-gz"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tail on .gz: got %v, want %v", got, want)
	}
}

// TestStreamResume checks the append-only resume protocol: frames carry the
// byte offset past each line, and offset=N streams only what follows N.
func TestStreamResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.log")
	if err := os.WriteFile(path, []byte("1\n2\n3\n"), 0o644); err != nil { // 6 bytes
		t.Fatal(err)
	}
	config = defaultConfig()
	config.Sources = []string{path}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	// Full read: offsets 2, 4, 6.
	resp, err := http.Get(srv.URL + "?mode=grep&path=" + url.QueryEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	frames := readSSEData(t, resp.Body, 3, 5*time.Second)
	resp.Body.Close()
	for i, wantPos := range []int64{2, 4, 6} {
		if frames[i].O != wantPos {
			t.Errorf("frame %d offset = %d, want %d", i, frames[i].O, wantPos)
		}
	}

	// Resume from byte 4: only the last line.
	resp, err = http.Get(srv.URL + "?mode=grep&offset=4&path=" + url.QueryEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	got := frameTexts(readSSEData(t, resp.Body, 1, 5*time.Second))
	resp.Body.Close()
	if !reflect.DeepEqual(got, []string{"3"}) {
		t.Fatalf("resume: got %v, want [3]", got)
	}
}

// TestStreamProgress checks that grep loads report byte progress and finish at
// done == total (the client's 0-100 bar).
func TestStreamProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.log")
	if err := os.WriteFile(path, []byte("1\n2\n3\n"), 0o644); err != nil { // 6 bytes
		t.Fatal(err)
	}
	config = defaultConfig()
	config.Sources = []string{path}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=grep&path=" + url.QueryEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // grep closes at EOF
	resp.Body.Close()
	if !strings.Contains(string(body), "event: progress") {
		t.Fatalf("no progress events in:\n%s", body)
	}
	if !strings.Contains(string(body), `{"d":6,"t":6}`) {
		t.Fatalf("progress did not reach done == total:\n%s", body)
	}
}

// TestArchiveProgress checks that viewing a compressed archive reports real
// progress — measured in compressed bytes against the on-disk size — and that
// its frames carry no resume offset (archives always restream whole).
func TestArchiveProgress(t *testing.T) {
	dir := writeArchives(t)
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	gz := filepath.Join(dir, "old.log.gz")
	fi, err := os.Stat(gz)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "?mode=grep&path=" + url.QueryEscape(gz))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // grep closes at EOF
	resp.Body.Close()
	want := fmt.Sprintf(`{"d":%d,"t":%d}`, fi.Size(), fi.Size())
	if !strings.Contains(string(body), want) {
		t.Fatalf("progress did not reach the archive's on-disk size %s:\n%s", want, body)
	}
	if strings.Contains(string(body), `"o":`) {
		t.Fatalf("archive frames must not carry resume offsets:\n%s", body)
	}
}

// TestStreamReset checks that an offset beyond the file (truncated or replaced
// since the client cached it) triggers a reset and a restart from the top.
func TestStreamReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.log")
	if err := os.WriteFile(path, []byte("1\n2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config = defaultConfig()
	config.Sources = []string{path}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=grep&offset=999&path=" + url.QueryEscape(path))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body) // grep closes at EOF, so the body ends
	resp.Body.Close()
	if !strings.Contains(string(body), "event: reset") {
		t.Fatalf("expected a reset event, got:\n%s", body)
	}
	if !strings.Contains(string(body), `"t":"1"`) {
		t.Fatalf("expected the stream to restart from the top, got:\n%s", body)
	}
}

// TestFindHandler checks the bounded server-side search: matches with context,
// the per-file cap, archive inclusion via stale=1, and input validation.
func TestFindHandler(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 1; i <= 40; i++ {
		if i == 15 || i == 30 {
			lines = append(lines, "needle here")
		} else {
			lines = append(lines, "filler")
		}
	}
	os.WriteFile(filepath.Join(dir, "a.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.log"), []byte("nothing to see\n"), 0o644)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("archived needle\n"))
	zw.Close()
	os.WriteFile(filepath.Join(dir, "old.log.gz"), gz.Bytes(), 0o644)

	config = defaultConfig()
	config.Sources = []string{dir}
	createListing(config.Sources)

	srv := httptest.NewServer(http.HandlerFunc(findHandler))
	defer srv.Close()

	get := func(params string) []findResult {
		t.Helper()
		resp, err := http.Get(srv.URL + "?" + params)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []findResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		return out
	}

	// Live files only: one file, two matches, full context on both sides.
	res := get("q=needle&all=1")
	if len(res) != 1 || !strings.HasSuffix(res[0].Path, "a.log") || len(res[0].Matches) != 2 {
		t.Fatalf("unexpected results: %+v", res)
	}
	m := res[0].Matches[0]
	if m.Text != "needle here" || len(m.Before) != findCtxLines || len(m.After) != findCtxLines {
		t.Fatalf("context wrong: before=%d after=%d text=%q", len(m.Before), len(m.After), m.Text)
	}

	// stale=1 also searches the archive, decoded.
	res = get("q=needle&all=1&stale=1")
	if len(res) != 2 {
		t.Fatalf("expected the archive to match too, got %+v", res)
	}

	// The per-file cap: a file where everything matches yields findMaxMatches.
	res = get("q=.&all=1")
	if len(res[0].Matches) != findMaxMatches {
		t.Fatalf("cap: got %d matches", len(res[0].Matches))
	}

	// Validation and allow-listing.
	for _, c := range []struct {
		params string
		code   int
	}{
		{"q=", http.StatusBadRequest},
		{"q=%28", http.StatusBadRequest}, // "(" — invalid regexp
		{"q=x&path=/etc/passwd", http.StatusNotFound},
	} {
		resp, err := http.Get(srv.URL + "?" + c.params)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%q: status %d, want %d", c.params, resp.StatusCode, c.code)
		}
	}
}

// TestStreamLive checks tail signals the end of its initial catch-up read, so
// the client can hide the loading bar shown just for the initial load.
func TestStreamLive(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")
	srv := httptest.NewServer(http.HandlerFunc(streamHandler))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?mode=tail&nlines=2&path=testdata/ex1/var/log/1.log")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	found := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: live" {
				found <- true
				return
			}
		}
		if err := scanner.Err(); err != nil {
			t.Errorf("reading SSE stream: %v", err)
		}
		found <- false
	}()
	select {
	case ok := <-found:
		if !ok {
			t.Fatal("stream ended without a live event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the live event")
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
		{"/vfs/main.js", "EventSource"},
		{"/vfs/main.css", "log-entry"},
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
	if !strings.Contains(body, `id="toolbar"`) || !strings.Contains(body, "vfs/main.js") {
		t.Fatal("index template did not render the expected content")
	}
	if !strings.Contains(body, `id="version"`) || !strings.Contains(body, ">dev<") {
		t.Fatal("index template did not render the version badge")
	}
}

func TestDownloadHandler(t *testing.T) {
	setupConfig(t, "testdata/ex1/var/log/1.log")

	// Allowed file: served, but as a no-sniff plain-text attachment so a log that
	// looks like HTML cannot execute as script in this origin.
	w := httptest.NewRecorder()
	downloadHandler(w, httptest.NewRequest("GET", "/files/?path=testdata/ex1/var/log/1.log", nil))
	if w.Code != http.StatusOK {
		t.Errorf("allowed download: status = %d", w.Code)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("download X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := w.Header().Get("Content-Disposition"); got != "attachment" {
		t.Errorf("download Content-Disposition = %q, want attachment", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("download Content-Type = %q, want text/plain", ct)
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
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func setupRoutes(relativeroot string) *http.ServeMux {
	router := http.NewServeMux()

	// Serve the two embedded web assets (see frontend.go). The Go template
	// living beside them is server-side input, not exposed.
	router.HandleFunc(relativeroot+"main.css", serveAsset("text/css; charset=utf-8", mainCSS))
	router.HandleFunc(relativeroot+"main.js", serveAsset("text/javascript; charset=utf-8", mainJS))
	router.HandleFunc(relativeroot+"list", listHandler)
	router.HandleFunc(relativeroot+"stream", streamHandler)
	router.HandleFunc(relativeroot+"find", findHandler)
	router.HandleFunc(relativeroot+"files/", downloadHandler)
	router.HandleFunc(relativeroot+"", indexHandler)

	return router
}

func setupServer(config *Config, addr string) *http.Server {
	router := setupRoutes(config.RelativeRoot)
	loggingRouter := loggingHandler(slog.Default(), router)

	server := http.Server{
		Addr:    addr,
		Handler: loggingRouter,
		// http.Server predates slog and wants a *log.Logger; bridge it.
		ErrorLog:    slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ReadTimeout: 5 * time.Second,
		// No WriteTimeout: the /stream endpoint serves long-lived Server-Sent
		// Events connections that a write deadline would abruptly cut off.
		IdleTimeout: 15 * time.Second,
	}

	return &server
}

// indexTemplate is parsed once at startup — the template is embedded in the
// binary, so it cannot change while the process runs.
var indexTemplate = template.Must(template.New("main.html").Parse(indexHTML))

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if err := indexTemplate.Execute(w, config); err != nil {
		slog.Error("rendering index", "err", err)
	}
}

// listHandler returns the current file listing as JSON. The frontend fetches it
// to populate the file selector.
func listHandler(w http.ResponseWriter, r *http.Request) {
	listing := createListing(config.Sources)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(listing); err != nil {
		slog.Error("writing file listing", "err", err)
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !fileAllowed(path) {
		slog.Warn("attempt to access unknown file", "path", path)
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
	// Log files routinely contain attacker-controlled text (User-Agents, URLs, …).
	// Serve them as a plain-text attachment with sniffing disabled, so a log line
	// that looks like HTML can't be rendered as script in this origin.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	http.ServeFile(w, r, path)
}

// serveAsset serves one embedded asset. Caching is disabled: a redeployed
// binary may carry different assets, so clients must always fetch fresh ones.
func serveAsset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
	}
}

// responseRecorder wraps an http.ResponseWriter to capture the status code and
// number of bytes written, for access logging. It implements Unwrap so that
// http.ResponseController (used by streamHandler to flush) can reach the
// underlying ResponseWriter.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// loggingHandler logs each request through logger, one INFO line per request.
// Streaming keeps working because the SSE handler flushes through
// http.ResponseController, which unwraps responseRecorder to reach the real
// writer. For long-lived SSE streams the line appears when the stream ends,
// with duration and bytes covering the whole stream.
func loggingHandler(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		logger.Info("request", "host", host, "method", r.Method,
			"uri", r.RequestURI, "status", rec.status, "bytes", rec.bytes,
			"duration", time.Since(start).Round(time.Millisecond))
	})
}

// findMaxMatches and findCtxLines are the default bounds of a search: the
// first N matches per file, each with C lines of context. The UI adjusts both
// per request (max ≤ 100, ctx ≤ 10 — see findHandler), but a bound always
// stands, and that is why find stays fast on huge logs — most scans stop long
// before the end of the file, and the response is small no matter how large
// the input. Find is a scent trail, not the full hunt: to see more than the
// first matches, open the file's view and step through the highlights there.
const (
	findMaxMatches = 3
	findCtxLines   = 3
)

// queryInt returns the named query parameter as an int clamped to at most hi,
// or def when the parameter is absent, malformed, or below lo. A bound always
// stands: clients pick values only within [lo, hi].
func queryInt(query url.Values, name string, def, lo, hi int) int {
	n, err := strconv.Atoi(query.Get(name))
	if err != nil || n < lo {
		return def
	}
	return min(n, hi)
}

// findMatch is one search hit with its surrounding lines.
type findMatch struct {
	Before []string `json:"before"`
	Text   string   `json:"text"`
	After  []string `json:"after"`
}

// findResult groups one file's hits.
type findResult struct {
	Path    string      `json:"path"`
	Matches []findMatch `json:"matches"`
}

// findHandler searches the selected files server-side and returns JSON — the
// fast alternative to streaming whole files to the browser. Query parameters:
// q (RE2 regexp), then path / all=1 / scope as in /stream; stale=1 also
// searches rotated archives (decoded transparently). The UI's search toggles
// arrive as literal=1 (match q as literal text, no regexp syntax), nocase=1
// (ignore case) and invert=1 (return the lines that do NOT match, grep -v);
// max and ctx (both bounded) adjust how many matches per file are returned
// and how many context lines surround each.
func findHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	q := query.Get("q")
	if q == "" {
		http.Error(w, "empty search", http.StatusBadRequest)
		return
	}
	if query.Get("literal") == "1" {
		q = regexp.QuoteMeta(q)
	}
	if query.Get("nocase") == "1" {
		q = "(?i)" + q
	}
	re, err := regexp.Compile(q)
	if err != nil {
		http.Error(w, "invalid search: "+err.Error(), http.StatusBadRequest)
		return
	}
	invert := query.Get("invert") == "1"

	var paths []string
	if query.Get("all") == "1" {
		paths = allowedFiles(query.Get("scope")) // "" scope selects everything
		if query.Get("stale") != "1" {
			paths = slices.DeleteFunc(paths, isStale)
		}
	} else {
		path := query.Get("path")
		if !fileAllowed(path) {
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		paths = []string{path}
	}

	// Scan the files concurrently; each stops at its match cap. The response
	// is NDJSON: progress lines {"d","t"} while the scan runs (driving the
	// client's 0-100 bar — a rare needle means reading everything), then one
	// final {"results": [...]} line. Progress counts on-disk bytes even for
	// archives: the counter sits under the decompressor.
	var total int64
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	// count=1 asks for whole-file match totals instead of excerpts: it backs
	// the view's "N in file" counter, where the browser highlights only the
	// lines it holds but the total must cover the entire file.
	counting := query.Get("count") == "1"
	// max adjusts the per-file excerpt cap (bounded): the UI's matches-per-file
	// select, and the view's "continue search", which lists up to 100 matches
	// of a single file instead of the default scent-trail few. ctx adjusts how
	// many lines of context surround each match (0 = just the matching lines).
	maxMatches := queryInt(query, "max", findMaxMatches, 1, 100)
	ctxLines := queryInt(query, "ctx", findCtxLines, 0, 10)
	matches := make([][]findMatch, len(paths))
	counts := make([]int64, len(paths))
	read := make([]int64, len(paths)) // per-file bytes consumed, updated atomically
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Go(func() {
			if counting {
				counts[i] = countInFile(r.Context(), p, re, invert, &read[i])
			} else {
				matches[i] = findInFile(r.Context(), p, re, invert, &read[i], maxMatches, ctxLines)
			}
		})
	}
	scansDone := make(chan struct{})
	go func() { wg.Wait(); close(scansDone) }()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	reportProgress(w, scansDone, read, total)

	if counting {
		type fileCount struct {
			Path string `json:"path"`
			N    int64  `json:"n"`
		}
		out := make([]fileCount, len(paths))
		for i, p := range paths {
			out[i] = fileCount{p, counts[i]}
		}
		if err := json.NewEncoder(w).Encode(struct {
			Counts []fileCount `json:"counts"`
		}{out}); err != nil {
			slog.Error("writing count results", "err", err)
		}
		return
	}
	results := []findResult{}
	for i, p := range paths {
		if len(matches[i]) > 0 {
			results = append(results, findResult{Path: p, Matches: matches[i]})
		}
	}
	if err := json.NewEncoder(w).Encode(struct {
		Results []findResult `json:"results"`
	}{results}); err != nil {
		slog.Error("writing find results", "err", err)
	}
}

// progressGauge tracks done/total bytes and reports when the integer percent
// moves, so a progress stream writes at most ~100 updates however large the
// input. Both progress protocols meter through it: /find's NDJSON lines and
// /stream's SSE frames.
type progressGauge struct {
	done, total int64
	pct         int // last percent reported; -1 so that 0% still reports
}

// step advances the gauge to pos and reports whether a new percent was crossed,
// clamped at 100 (data appended after connect would push past the total).
func (g *progressGauge) step(pos int64) bool {
	if g.total <= 0 || pos < g.done {
		return false
	}
	g.done = pos
	pct := min(int(pos*100/g.total), 100)
	if pct == g.pct {
		return false
	}
	g.pct = pct
	return true
}

// reportProgress writes an NDJSON progress line {"d":done,"t":total} at most
// every 150ms until done closes, each flushed immediately so the client's
// 0-100 bar moves while the scan runs (a rare needle means reading everything).
// read holds per-file byte counters the scan goroutines advance atomically.
func reportProgress(w http.ResponseWriter, scansDone <-chan struct{}, read []int64, total int64) {
	if total <= 0 {
		<-scansDone
		return
	}
	rc := http.NewResponseController(w)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	gauge := progressGauge{total: total, pct: -1}
	for {
		select {
		case <-scansDone:
			return
		case <-ticker.C:
			var done int64
			for i := range read {
				done += atomic.LoadInt64(&read[i])
			}
			if gauge.step(done) {
				fmt.Fprintf(w, "{\"d\":%d,\"t\":%d}\n", done, total)
				rc.Flush()
			}
		}
	}
}

// scanLines reads path line by line — decoded if compressed, with consumed
// on-disk bytes counted into nRead for progress — and calls fn per line until
// fn returns false, the file ends, or the request is cancelled (the client
// navigated away; don't finish a scan nobody reads).
func scanLines(ctx context.Context, path string, nRead *int64, fn func(line string) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// The counter sits between the file and the decoder, so progress measures
	// on-disk bytes for plain and compressed files alike.
	var rd io.Reader = &countingReader{f, nRead}
	if d := decoder(path); d != nil {
		if rd, err = d(rd); err != nil {
			return
		}
		defer closeDecoder(rd)
	}

	br := bufio.NewReaderSize(rd, 64*1024)
	for i := 0; ; i++ {
		if i%1024 == 0 && ctx.Err() != nil {
			return
		}
		line, err := readLine(br) // bounded: over-long lines arrive in pieces
		if (line != "" || err == nil) && !fn(line) {
			return
		}
		if err != nil {
			return
		}
	}
}

// findInFile returns the first maxMatches hits in the file (with invert, the
// lines that do NOT match), each with up to ctxLines lines of context on
// both sides, stopping as soon as the last hit's after-context is complete —
// a scan rarely reads the whole file. A hit inside a previous hit's
// after-context merges into that excerpt instead of starting its own.
func findInFile(ctx context.Context, path string, re *regexp.Regexp, invert bool, nRead *int64, maxMatches, ctxLines int) []findMatch {
	var ring []string // the last ctxLines lines, before-context for the next hit
	var hits []findMatch
	scanLines(ctx, path, nRead, func(line string) bool {
		// Every line first serves as after-context for the open hits. A
		// matching line consumed that way is already shown (the client
		// highlights matches wherever they appear, context included), so it
		// does not open an excerpt of its own — that would render it twice.
		complete := true
		shown := false
		for i := range hits {
			if len(hits[i].After) < ctxLines {
				hits[i].After = append(hits[i].After, line)
				complete = complete && len(hits[i].After) == ctxLines
				shown = true
			}
		}
		if !shown && len(hits) < maxMatches && re.MatchString(line) != invert {
			hits = append(hits, findMatch{
				Before: append([]string{}, ring...),
				Text:   line,
				After:  []string{},
			})
			complete = ctxLines == 0 // with context, wait for this hit's after-lines
		}
		if ring = append(ring, line); len(ring) > ctxLines {
			ring = ring[1:]
		}
		return len(hits) < maxMatches || !complete // done: cap reached, contexts full
	})
	return hits
}

// countInFile returns how many lines match re (with invert, how many do NOT)
// — the whole file, uncapped (unlike findInFile there is no early exit; a
// count must cover everything).
func countInFile(ctx context.Context, path string, re *regexp.Regexp, invert bool, nRead *int64) int64 {
	var n int64
	scanLines(ctx, path, nRead, func(line string) bool {
		if re.MatchString(line) != invert {
			n++
		}
		return true
	})
	return n
}

// mergeInterval is how often all-files mode flushes its buffer of lines, sorted
// by timestamp, so several files are interleaved chronologically.
const mergeInterval = 200 * time.Millisecond

// sseHeartbeat is how often a quiet stream writes an SSE comment frame, so
// proxies with idle timeouts (nginx defaults to 60s) keep the connection open.
const sseHeartbeat = 25 * time.Second

// ringTrimChunk is how much slack a capped view's tail ring accumulates before
// it is trimmed back to the cap in one copy.
const ringTrimChunk = 1000

// logLine is one line of output tagged with the file it came from (used to
// prefix lines when several files are streamed at once) and the byte offset
// just past it (-1 when unknown; used by the client's resume cache).
type logLine struct {
	path string
	text string
	pos  int64
}

// streamHandler streams a file (or every served file, with all=1) to the client
// over Server-Sent Events. Query parameters:
//
//	mode    "tail" (default) follows the file like tail -f; "view" reads the
//	        whole file once, from the start, without following — the UI's
//	        "view", single files only. Aggregate streams are always tailed
//	        and skip rotated/compressed files (search the archives with /find).
//	nlines  in tail mode, how many trailing lines to show before following. In
//	        view mode, a cap: at most the last nlines lines are sent — the
//	        client discards anything past its scrollback anyway, so a huge
//	        archive doesn't push millions of lines over the wire. The whole
//	        file is still read (that is what drives the progress bar); 0 sends
//	        every line.
//	path    the file to stream, or all=1 for every served file.
//	scope   with all=1, limit the stream to files under this directory prefix.
//	offset  resume a single-file stream from this byte offset. Served files
//	        only grow, so the client caches lines it has seen and re-requests
//	        just the remainder. If the file shrank or was replaced since, an
//	        "event: reset" frame tells the client to drop its cache, and the
//	        stream restarts from the beginning.
//
// Each line is one SSE "data:" frame holding a JSON object: "t" is the line's
// text, "p" (multi-file streams) the file it came from, and "o" (single-file
// streams) the byte offset to resume from after this line. Reading and
// following are all done in Go; searching the lines happens in the browser.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	mode := query.Get("mode")
	if mode != "" && mode != "tail" && mode != "view" {
		http.Error(w, "unknown mode: "+mode, http.StatusBadRequest)
		return
	}
	follow := mode != "view" // "tail" (default) follows; "view" reads once — a live view arrives as tail (the client upgrades it)
	nlines, _ := strconv.Atoi(query.Get("nlines"))

	// Resolve the files to stream. "all=1" tails every served file at once —
	// viewing a merged dump of several files is not useful, so it is
	// limited to single files. Rotated/compressed leftovers are skipped in
	// aggregate streams: tailing them is meaningless and their raw bytes are
	// garbage.
	var paths []string
	if query.Get("all") == "1" {
		if !follow {
			http.Error(w, "view is limited to single files", http.StatusBadRequest)
			return
		}
		paths = slices.DeleteFunc(allowedFiles(query.Get("scope")), isStale) // "" scope selects everything
		if len(paths) == 0 {
			http.Error(w, "no files to stream", http.StatusNotFound)
			return
		}
	} else {
		path := query.Get("path")
		if !fileAllowed(path) {
			slog.Warn("attempt to stream unknown file", "path", path)
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		if isStale(path) {
			follow = false // a rotation leftover never grows; force one read-once pass
		}
		paths = []string{path}
	}

	// Resume support (single-file streams only): the client sends the byte
	// offset it has cached up to, plus the file identity it read it from (the
	// "event: id" frame below). The cache is invalid — signal reset and start
	// over — when the file shrank below the offset, or when a different file
	// lives at the path now: a rotated-in replacement may already have grown
	// past the offset, so size alone cannot tell.
	start, reset := int64(-1), false
	if v := query.Get("offset"); v != "" && len(paths) == 1 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			start = n
			fi, err := os.Stat(paths[0])
			if err != nil || fi.Size() < start ||
				(query.Get("id") != "" && query.Get("id") != fileID(fi)) {
				start, reset = -1, true
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)

	s := &sseStream{
		w:          w,
		rc:         http.NewResponseController(w),
		multi:      len(paths) > 1,
		compressed: decoder(paths[0]) != nil, // single-file: a decoded archive?
		catchingUp: len(paths),
		gauge:      progressGauge{pct: -1},
	}
	s.rc.Flush()

	// A single-file stream announces the file's identity first: the client
	// stores it with its cached offset and echoes it on resume (see above).
	if !s.multi {
		if fi, err := os.Stat(paths[0]); err == nil {
			if id := fileID(fi); id != "" {
				fmt.Fprintf(w, "event: id\ndata: %s\n\n", id)
			}
		}
	}
	if reset {
		s.event("reset")
	}

	// Progress applies to view loads (single-file by construction — aggregate
	// streams always follow) and to a follow stream resuming from a known
	// offset (a cached view or tail): either way the read is bounded by the
	// file's on-disk size at connect time, so it can report 0-100 — lines
	// appended later just stay pinned at 100. For compressed archives each
	// line's pos is the compressed-side position (see streamCompressed), so
	// measured against the same on-disk size they too get a real bar.
	if !follow || start >= 0 {
		if fi, err := os.Stat(paths[0]); err == nil {
			s.gauge.total = fi.Size()
		}
	}

	// Stream every file concurrently into a shared channel.
	ctx := r.Context()
	lines := make(chan logLine, 256)
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Go(func() {
			streamFile(ctx, p, follow, nlines, start, func(text string, pos int64) {
				select {
				case lines <- logLine{p, text, pos}:
				case <-ctx.Done():
				}
			})
		})
	}
	go func() { wg.Wait(); close(lines) }()

	if s.multi {
		streamMerged(ctx, s, lines, nlines)
	} else {
		streamSingle(ctx, s, lines, !follow && nlines > 0, nlines, paths[0])
	}
}

// sseStream is the write side of one /stream response: SSE framing, the
// once-per-file catch-up accounting, and the progress gauge.
type sseStream struct {
	w          http.ResponseWriter
	rc         *http.ResponseController
	multi      bool // several files: frames carry the path, offsets are meaningless
	compressed bool // a decoded archive: pos is compressed-side progress, not a resume offset
	catchingUp int  // files still reading their initial backlog
	gauge      progressGauge
}

// event writes a named SSE control frame ("reset", "live", "eof") and flushes
// it out.
func (s *sseStream) event(name string) {
	fmt.Fprintf(s.w, "event: %s\ndata: \n\n", name)
	s.rc.Flush()
}

// writeLine sends one line as an SSE data frame; false means the client is
// gone. No flush here: during bulk reads a flush per line means a syscall and
// a tiny packet per line — the callers flush once their batch is done.
func (s *sseStream) writeLine(ln logLine) bool {
	frame := struct {
		Path string `json:"p,omitempty"` // set when several files are streamed
		Text string `json:"t"`
		Pos  int64  `json:"o,omitempty"` // resume offset, single-file streams only
	}{Text: ln.text}
	if s.multi {
		frame.Path = ln.path
	} else if ln.pos > 0 && !s.compressed {
		frame.Pos = ln.pos
	}
	data, _ := json.Marshal(frame)
	_, err := fmt.Fprintf(s.w, "data: %s\n\n", data)
	return err == nil
}

// caughtUp consumes the marker each file sends once its initial catch-up read
// is done; after the last one the client may hide its loading bar. This only
// concerns the initial load — the stream then just keeps following.
func (s *sseStream) caughtUp(ln logLine) bool {
	if ln.pos != posCaughtUp {
		return false
	}
	if s.catchingUp--; s.catchingUp == 0 {
		s.event("live")
	}
	return true
}

// reset tells the client to drop everything it holds for this stream — the
// file at path was rotated or truncated mid-stream — and announces the
// identity of the file that lives there now.
func (s *sseStream) reset(path string) {
	s.event("reset")
	if fi, err := os.Stat(path); err == nil {
		if id := fileID(fi); id != "" {
			fmt.Fprintf(s.w, "event: id\ndata: %s\n\n", id)
			s.rc.Flush()
		}
	}
}

// heartbeat writes an SSE comment frame, which EventSource ignores; false
// means the client is gone.
func (s *sseStream) heartbeat() bool {
	if _, err := fmt.Fprint(s.w, ": hb\n\n"); err != nil {
		return false
	}
	s.rc.Flush()
	return true
}

// progress advances the gauge to pos and emits an SSE progress frame when it
// crosses a new percent; false means the client is gone.
func (s *sseStream) progress(pos int64) bool {
	if !s.gauge.step(pos) {
		return true
	}
	if _, err := fmt.Fprintf(s.w, "event: progress\ndata: {\"d\":%d,\"t\":%d}\n\n", s.gauge.done, s.gauge.total); err != nil {
		return false
	}
	s.rc.Flush()
	return true
}

// streamSingle sends one file's lines as they arrive — a single file is
// already in order. In view mode with an nlines cap, lines are instead
// collected in a tail ring and sent once the read finishes.
func streamSingle(ctx context.Context, s *sseStream, lines <-chan logLine, capped bool, nlines int, path string) {
	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	var ring []logLine
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			if !s.heartbeat() {
				return
			}
		case ln, ok := <-lines:
			if !ok {
				if capped && len(ring) > nlines {
					ring = ring[len(ring)-nlines:]
				}
				for _, ln := range ring {
					if !s.writeLine(ln) {
						return
					}
				}
				// The file is fully read (view mode): tell the client so its
				// EventSource closes instead of reconnecting.
				s.event("eof")
				return
			}
			if ln.pos == posReset {
				s.reset(path) // rotated/truncated: the client starts over
				continue
			}
			if s.caughtUp(ln) {
				continue
			}
			if !s.progress(ln.pos) {
				return
			}
			if capped {
				// Keep only the stream's tail, trimming in chunks so the cost
				// stays amortized rather than per line.
				if ring = append(ring, ln); len(ring) >= nlines+ringTrimChunk {
					ring = append(ring[:0], ring[len(ring)-nlines:]...)
				}
				continue
			}
			if !s.writeLine(ln) {
				return
			}
			// Per-line flushing matters only when following live — during a
			// bulk load it would mean a tiny packet per line. Bulk loads flush
			// via the progress events and at EOF; in between, the response
			// buffer streams larger loads on its own.
			if s.catchingUp == 0 && len(lines) == 0 {
				s.rc.Flush() // live and drained: push the line out now
			}
		}
	}
}

// streamMerged merges several files in timestamp order. Lines are buffered and
// flushed sorted every mergeInterval, which also orders the initial burst. A
// line with no recognizable timestamp inherits its file's last one, so
// multi-line entries stay together; failing that it sorts as "now".
func streamMerged(ctx context.Context, s *sseStream, lines <-chan logLine, nlines int) {
	type timedLine struct {
		logLine
		ts time.Time
	}
	var buf []timedLine
	ticker := time.NewTicker(mergeInterval)
	defer ticker.Stop()

	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		sort.SliceStable(buf, func(i, j int) bool { return buf[i].ts.Before(buf[j].ts) })
		for _, ln := range buf {
			if !s.writeLine(ln.logLine) {
				return false
			}
		}
		buf = buf[:0]
		s.rc.Flush() // one flush per merge batch, not per line
		return true
	}

	// Per-file timestamp detection (see timestamper in tailer.go): each file's
	// stamper is primed from the file itself — the format from its trailing
	// lines, and the last date before the backlog window, so a backlog that
	// starts mid-entry (undated continuation lines) still inherits its entry's
	// date instead of sorting as "now".
	stampers := make(map[string]*timestamper)
	timestamp := func(ln logLine) time.Time {
		t := stampers[ln.path]
		if t == nil {
			t = primeTimestamper(ln.path, nlines)
			stampers[ln.path] = t
		}
		return t.stamp(ln.text)
	}

	hb := time.NewTicker(sseHeartbeat)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			if !s.heartbeat() {
				return
			}
		case <-ticker.C:
			if !flush() {
				return
			}
		case ln, ok := <-lines:
			if !ok {
				// Aggregate streams always follow, so the readers — and with
				// them this channel — only wind down once ctx is cancelled.
				return
			}
			if ln.pos == posReset {
				continue // one rotated file must not wipe a merged view
			}
			if s.caughtUp(ln) {
				continue
			}
			buf = append(buf, timedLine{ln, timestamp(ln)})
		}
	}
}

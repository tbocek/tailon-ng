package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func setupRoutes(relativeroot string) *http.ServeMux {
	router := http.NewServeMux()

	// Serve the embedded frontend assets (see frontend.go). Only the two real
	// web assets are exposed: the Go templates living beside them are
	// server-side input, and there is no directory to browse.
	staticHandler := noCacheControl(http.FileServerFS(frontendAssets))
	staticHandler = http.StripPrefix(relativeroot+"vfs/", staticHandler)

	router.HandleFunc(relativeroot+"vfs/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, relativeroot+"vfs/") {
		case "main.css", "main.js":
			staticHandler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	router.HandleFunc(relativeroot+"list", listHandler)
	router.HandleFunc(relativeroot+"stream", streamHandler)
	router.HandleFunc(relativeroot+"find", findHandler)
	router.HandleFunc(relativeroot+"files/", downloadHandler)
	router.HandleFunc(relativeroot+"", indexHandler)

	return router
}

func setupServer(config *Config, addr string, logger *log.Logger) *http.Server {
	router := setupRoutes(config.RelativeRoot)
	loggingRouter := loggingHandler(os.Stderr, router)

	server := http.Server{
		Addr:        addr,
		Handler:     loggingRouter,
		ErrorLog:    logger,
		ReadTimeout: 5 * time.Second,
		// No WriteTimeout: the /stream endpoint serves long-lived Server-Sent
		// Events connections that a write deadline would abruptly cut off.
		IdleTimeout: 15 * time.Second,
	}

	return &server
}

// indexTemplate is parsed once at startup — the templates are embedded in the
// binary, so they cannot change while the process runs.
var indexTemplate = template.Must(template.ParseFS(frontendAssets, "base.html", "tailon.html"))

func indexHandler(w http.ResponseWriter, r *http.Request) {
	indexTemplate.Execute(w, config)
}

// listHandler returns the current file listing as JSON. The frontend fetches it
// to populate the file selector. This replaces the SockJS "list" message.
func listHandler(w http.ResponseWriter, r *http.Request) {
	listing := createListing(config.Sources)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(listing); err != nil {
		log.Println("error: ", err)
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !fileAllowed(path) {
		log.Printf("warn: attempt to access unknown file: %q", path)
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

func noCacheControl(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		h.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
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

// loggingHandler logs each request in Apache Common Log Format. It replaces
// github.com/gorilla/handlers; streaming keeps working because the SSE handler
// flushes through http.ResponseController, which unwraps to the real writer.
func loggingHandler(out io.Writer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		fmt.Fprintf(out, "%s - - [%s] %q %d %d\n",
			host, start.Format("02/Jan/2006:15:04:05 -0700"),
			r.Method+" "+r.RequestURI+" "+r.Proto, rec.status, rec.bytes)
	})
}

// findMaxMatches and findCtxLines bound a search: the first N matches per
// file, each with C lines of context. The bound is why find stays fast on huge
// logs — most scans stop long before the end of the file, and the response is
// small no matter how large the input.
const (
	findMaxMatches = 10
	findCtxLines   = 10
)

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
// searches rotated archives (decoded transparently).
func findHandler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("q") == "" {
		http.Error(w, "empty search", http.StatusBadRequest)
		return
	}
	re, err := regexp.Compile(query.Get("q"))
	if err != nil {
		http.Error(w, "invalid search: "+err.Error(), http.StatusBadRequest)
		return
	}

	var paths []string
	if query.Get("all") == "1" {
		if scope := query.Get("scope"); scope != "" {
			paths = allowedUnder(scope)
		} else {
			paths = allowedFiles()
		}
		if query.Get("stale") != "1" {
			live := paths[:0]
			for _, p := range paths {
				if !isStale(p) {
					live = append(live, p)
				}
			}
			paths = live
		}
	} else {
		path := query.Get("path")
		if !fileAllowed(path) {
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		paths = []string{path}
	}

	// Scan the files concurrently; each stops at its match cap.
	matches := make([][]findMatch, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) { defer wg.Done(); matches[i] = findInFile(r.Context(), p, re) }(i, p)
	}
	wg.Wait()

	results := []findResult{}
	for i, p := range paths {
		if len(matches[i]) > 0 {
			results = append(results, findResult{Path: p, Matches: matches[i]})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// findInFile returns the first findMaxMatches hits in the file (decoded if
// compressed), each with up to findCtxLines lines of context on both sides. It
// reads line-buffered and returns as soon as the last hit's after-context is
// complete, so a scan rarely reads the whole file. A cancelled request (the
// client navigated away) stops the scan instead of running it to the end.
func findInFile(ctx context.Context, path string, re *regexp.Regexp) []findMatch {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rd io.Reader = f
	if d := decoder(path); d != nil {
		if rd, err = d(f); err != nil {
			return nil
		}
		defer closeDecoder(rd)
	}

	br := bufio.NewReader(rd)
	var ring []string // the last findCtxLines lines, before-context for the next hit
	var hits []findMatch
	for n := 0; ; n++ {
		if n%1024 == 0 && ctx.Err() != nil {
			return hits
		}
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" || err == nil {
			// Every line first serves as after-context for the open hits (a
			// matching line inside another's context included, like grep -C).
			complete := true
			for i := range hits {
				if len(hits[i].After) < findCtxLines {
					hits[i].After = append(hits[i].After, line)
					complete = complete && len(hits[i].After) == findCtxLines
				}
			}
			if len(hits) < findMaxMatches && re.MatchString(line) {
				hits = append(hits, findMatch{
					Before: append([]string{}, ring...),
					Text:   line,
					After:  []string{},
				})
				complete = false
			}
			if ring = append(ring, line); len(ring) > findCtxLines {
				ring = ring[1:]
			}
			if len(hits) == findMaxMatches && complete {
				break // cap reached and every context is full: stop reading
			}
		}
		if err != nil {
			return hits
		}
	}
	return hits
}

// mergeInterval is how often all-files mode flushes its buffer of lines, sorted
// by timestamp, so several files are interleaved chronologically.
const mergeInterval = 200 * time.Millisecond

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
//	mode    "tail" (default) follows the file like tail -f; "grep" reads the
//	        whole file once, from the start, without following — the UI's
//	        "view", single files only. Aggregate streams are always tailed
//	        and skip rotated/compressed files (grep the archives with /find).
//	filter  optional regular expression; only matching lines are sent.
//	nlines  in tail mode, how many trailing lines to show before following. In
//	        view mode, a cap: at most the last nlines (filtered) lines are
//	        sent — the client discards anything past its scrollback anyway, so
//	        a huge archive doesn't push millions of lines over the wire. The
//	        whole file is still read (that is what drives the progress bar);
//	        0 sends every line.
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
// streams) the byte offset to resume from after this line. Reading, following
// and filtering are all done in Go.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	query := r.URL.Query()

	mode := query.Get("mode")
	if mode != "" && mode != "tail" && mode != "grep" {
		http.Error(w, "unknown mode: "+mode, http.StatusBadRequest)
		return
	}
	follow := mode != "grep" // "tail" (default) follows; "grep" reads once
	nlines, _ := strconv.Atoi(query.Get("nlines"))

	var filter *regexp.Regexp
	if expr := query.Get("filter"); expr != "" {
		re, err := regexp.Compile(expr)
		if err != nil {
			http.Error(w, "invalid filter: "+err.Error(), http.StatusBadRequest)
			return
		}
		filter = re
	}

	// Resolve the files to stream. "all=1" tails every served file at once —
	// viewing (grep) a merged dump of several files is not useful, so it is
	// limited to single files. Rotated/compressed leftovers are skipped in
	// aggregate streams: tailing them is meaningless and their raw bytes are
	// garbage.
	var paths []string
	if query.Get("all") == "1" {
		if !follow {
			http.Error(w, "view is limited to single files", http.StatusBadRequest)
			return
		}
		if scope := query.Get("scope"); scope != "" {
			paths = allowedUnder(scope) // just the files under one subfolder
		} else {
			paths = allowedFiles()
		}
		live := paths[:0]
		for _, p := range paths {
			if !isStale(p) {
				live = append(live, p)
			}
		}
		paths = live
		if len(paths) == 0 {
			http.Error(w, "no files to stream", http.StatusNotFound)
			return
		}
	} else {
		path := query.Get("path")
		if !fileAllowed(path) {
			log.Printf("warn: attempt to stream unknown file: %q", path)
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		if isStale(path) {
			follow = false // a rotation leftover never grows; force one grep pass
		}
		paths = []string{path}
	}

	// Resume support (single-file streams only): the client sends the byte
	// offset it has cached up to. A file smaller than that offset was truncated
	// or replaced, so the cache is invalid — signal reset and start over.
	start, reset := int64(-1), false
	if v := query.Get("offset"); v != "" && len(paths) == 1 {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			start = n
			if fi, err := os.Stat(paths[0]); err != nil || fi.Size() < start {
				start, reset = -1, true
			}
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	if reset {
		fmt.Fprint(w, "event: reset\ndata: \n\n")
		rc.Flush()
	}

	ctx := r.Context()
	multi := len(paths) > 1

	// Progress for view loads (single-file by construction — aggregate streams
	// always follow): total is the file's on-disk size, and each line's pos
	// advances the "done" count — including lines the filter drops, since
	// progress measures bytes read, not lines shown. For compressed archives
	// pos is the compressed-side position (see streamCompressed), so measured
	// against the same on-disk size they too get a real 0-100 bar.
	var progTotal, progDone int64
	progPct := -1
	if !follow {
		if fi, err := os.Stat(paths[0]); err == nil {
			progTotal = fi.Size()
		}
	}
	progress := func(pos int64) bool {
		if progTotal <= 0 || pos <= progDone {
			return true
		}
		progDone = pos
		if pct := int(progDone * 100 / progTotal); pct != progPct {
			progPct = pct
			if _, err := fmt.Fprintf(w, "event: progress\ndata: {\"d\":%d,\"t\":%d}\n\n", progDone, progTotal); err != nil {
				return false
			}
			rc.Flush()
		}
		return true
	}

	// Stream every file concurrently into a shared channel.
	lines := make(chan logLine, 256)
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			streamFile(ctx, p, follow, nlines, start, func(text string, pos int64) {
				select {
				case lines <- logLine{p, text, pos}:
				case <-ctx.Done():
				}
			})
		}(p)
	}
	go func() { wg.Wait(); close(lines) }()

	matches := func(text string) bool {
		return filter == nil || filter.MatchString(text)
	}
	// In tail mode, each file reports once when its initial catch-up read is
	// done; after the last one the client may hide its loading bar. This only
	// concerns the initial load — the stream then just keeps following.
	catchingUp := len(paths)
	caughtUp := func(ln logLine) bool {
		if ln.pos != posCaughtUp {
			return false
		}
		if catchingUp--; catchingUp == 0 {
			fmt.Fprint(w, "event: live\ndata: \n\n")
			rc.Flush()
		}
		return true
	}
	compressed := decoder(paths[0]) != nil // single-file: is it a decoded archive?
	writeLine := func(ln logLine) bool {
		frame := struct {
			Path string `json:"p,omitempty"` // set when several files are streamed
			Text string `json:"t"`
			Pos  int64  `json:"o,omitempty"` // resume offset, single-file streams only
		}{Text: ln.text}
		if multi {
			frame.Path = ln.path
		} else if ln.pos > 0 && !compressed {
			frame.Pos = ln.pos // an archive's pos is compressed-side progress, not a resume offset
		}
		data, _ := json.Marshal(frame)
		// No flush here: during bulk reads a flush per line means a syscall and
		// a tiny packet per line. The callers flush once their batch is done.
		_, err := fmt.Fprintf(w, "data: %s\n\n", data)
		return err == nil
	}
	// A single file is already in order, so stream its lines as they arrive.
	// In view mode with an nlines cap, matching lines are instead collected in
	// a tail ring and sent once the read finishes.
	if !multi {
		var ring []logLine
		capped := !follow && nlines > 0
		for {
			select {
			case <-ctx.Done():
				return
			case ln, ok := <-lines:
				if !ok {
					if capped && len(ring) > nlines {
						ring = ring[len(ring)-nlines:]
					}
					for _, ln := range ring {
						if !writeLine(ln) {
							return
						}
					}
					// The file is fully read (view mode): tell the client so
					// its EventSource closes instead of reconnecting.
					fmt.Fprint(w, "event: eof\ndata: \n\n")
					rc.Flush()
					return
				}
				if caughtUp(ln) {
					continue
				}
				if !progress(ln.pos) {
					return
				}
				if !matches(ln.text) {
					continue
				}
				if capped {
					// Keep only the stream's tail, trimming in chunks so the
					// cost stays amortized rather than per line.
					if ring = append(ring, ln); len(ring) >= nlines+ringTrimChunk {
						ring = append(ring[:0], ring[len(ring)-nlines:]...)
					}
					continue
				}
				if !writeLine(ln) {
					return
				}
				// Per-line flushing matters only when following live — during
				// a bulk load it would mean a tiny packet per line. Bulk loads
				// flush via the progress events and at EOF; in between, the
				// response buffer streams larger loads on its own.
				if catchingUp == 0 && len(lines) == 0 {
					rc.Flush() // live and drained: push the line out now
				}
			}
		}
	}

	// Several files: merge them in timestamp order. Lines are buffered and
	// flushed sorted every mergeInterval, which also orders the initial burst.
	// A line with no recognizable timestamp inherits its file's last one, so
	// multi-line entries stay together; failing that it sorts as "now".
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
			if !writeLine(ln.logLine) {
				return false
			}
		}
		buf = buf[:0]
		rc.Flush() // one flush per merge batch, not per line
		return true
	}

	// Per-file timestamp detection (see timestamper in tailer.go): the format is
	// detected from each file's first lines and then reused; a line with no
	// timestamp inherits its file's previous one, so multi-line entries stay
	// together.
	stampers := make(map[string]*timestamper)
	timestamp := func(ln logLine) time.Time {
		t := stampers[ln.path]
		if t == nil {
			t = &timestamper{}
			stampers[ln.path] = t
		}
		return t.stamp(ln.text)
	}

	for {
		select {
		case <-ctx.Done():
			return
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
			if caughtUp(ln) {
				continue
			}
			if !matches(ln.text) {
				continue
			}
			buf = append(buf, timedLine{ln, timestamp(ln)})
		}
	}
}

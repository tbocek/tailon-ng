package main

import (
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
	"sync"
	"time"
)

func setupRoutes(relativeroot string) *http.ServeMux {
	router := http.NewServeMux()

	// Serve the embedded frontend assets (see frontend.go).
	staticHandler := noCacheControl(http.FileServerFS(frontendAssets))
	staticHandler = http.StripPrefix(relativeroot+"vfs/", staticHandler)

	router.Handle(relativeroot+"vfs/", staticHandler)
	router.HandleFunc(relativeroot+"list", listHandler)
	router.HandleFunc(relativeroot+"stream", streamHandler)
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

func indexHandler(w http.ResponseWriter, r *http.Request) {
	t := template.Must(template.ParseFS(frontendAssets, "templates/base.html", "templates/tailon.html"))
	t.Execute(w, config)
}

// listHandler returns the current file listing as JSON. The frontend fetches it
// to populate the file selector. This replaces the SockJS "list" message.
func listHandler(w http.ResponseWriter, r *http.Request) {
	listing := createListing(config.FileSpecs)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(listing); err != nil {
		log.Println("error: ", err)
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	if !config.AllowDownload {
		http.Error(w, "downloads forbidden by server", http.StatusForbidden)
		return
	}

	path := r.URL.Query().Get("path")
	if !fileAllowed(path) {
		log.Printf("warn: attempt to access unknown file: %s", path)
		http.Error(w, "unknown file", http.StatusNotFound)
		return
	}
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

// mergeInterval is how often all-files mode flushes its buffer of lines, sorted
// by timestamp, so several files are interleaved chronologically.
const mergeInterval = 200 * time.Millisecond

// logLine is one line of output tagged with the file it came from (used to
// prefix lines when several files are streamed at once).
type logLine struct {
	path string
	text string
}

// streamHandler streams a file (or every served file, with all=1) to the client
// over Server-Sent Events. Query parameters:
//
//	mode    "tail" (default) follows the file like tail -f; "grep" reads the
//	        whole file once, from the start, without following.
//	filter  optional regular expression; only matching lines are sent.
//	invert  "1" inverts the filter, sending only non-matching lines.
//	nlines  in tail mode, how many trailing lines to show before following.
//	path    the file to stream, or all=1 for every served file.
//
// Each line is sent as an SSE "data:" frame holding the JSON-encoded line.
// Reading, following and filtering are all done in Go — no external tail/grep.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	query := r.URL.Query()

	follow := query.Get("mode") != "grep" // "tail" follows; "grep" reads once
	nlines, _ := strconv.Atoi(query.Get("nlines"))
	invert := query.Get("invert") == "1"

	var filter *regexp.Regexp
	if expr := query.Get("filter"); expr != "" {
		re, err := regexp.Compile(expr)
		if err != nil {
			http.Error(w, "invalid filter: "+err.Error(), http.StatusBadRequest)
			return
		}
		filter = re
	}

	// Resolve the files to stream. "all=1" streams every served file at once.
	var paths []string
	if query.Get("all") == "1" {
		paths = allowedFiles()
		if len(paths) == 0 {
			http.Error(w, "no files to stream", http.StatusNotFound)
			return
		}
	} else {
		path := query.Get("path")
		if !fileAllowed(path) {
			log.Print("Unknown file: ", path)
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		paths = []string{path}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	rc.Flush()

	ctx := r.Context()
	multi := len(paths) > 1

	// Stream every file concurrently into a shared channel.
	lines := make(chan logLine, 256)
	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			streamFile(ctx, p, follow, nlines, func(text string) {
				select {
				case lines <- logLine{p, text}:
				case <-ctx.Done():
				}
			})
		}(p)
	}
	go func() { wg.Wait(); close(lines) }()

	matches := func(text string) bool {
		return filter == nil || filter.MatchString(text) != invert
	}
	writeLine := func(ln logLine) bool {
		text := ln.text
		if multi {
			text = ln.path + ": " + text
		}
		data, _ := json.Marshal(text)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		rc.Flush()
		return true
	}
	writeEOF := func() {
		// Tell the client we're done so its EventSource closes instead of
		// reconnecting (only happens in grep mode, when every file is read).
		fmt.Fprint(w, "event: eof\ndata: \n\n")
		rc.Flush()
	}

	// A single file is already in order, so stream its lines as they arrive.
	if !multi {
		for {
			select {
			case <-ctx.Done():
				return
			case ln, ok := <-lines:
				if !ok {
					writeEOF()
					return
				}
				if matches(ln.text) && !writeLine(ln) {
					return
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
		sort.SliceStable(buf, func(i, j int) bool { return buf[i].ts.Before(buf[j].ts) })
		for _, ln := range buf {
			if !writeLine(ln.logLine) {
				return false
			}
		}
		buf = buf[:0]
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
				flush()
				writeEOF()
				return
			}
			if !matches(ln.text) {
				continue
			}
			buf = append(buf, timedLine{ln, timestamp(ln)})
		}
	}
}

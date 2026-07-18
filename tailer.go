package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/gzip" // drop-in, decodes ~2x faster than compress/gzip
	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// pollInterval is how often follow mode checks a file for newly appended data.
const pollInterval = 250 * time.Millisecond

// posCaughtUp, sent once per followed file as a line's pos, marks the end of
// its initial catch-up read: from here on, only newly appended data follows.
// The server turns the last one into an SSE "live" event so the client can
// hide its loading bar.
const posCaughtUp = -2

// streamFile sends each line of the file at path to send, along with the byte
// offset just past that line (-1 when unknown) — the client caches lines and
// resumes from the offset, since served files only ever grow. In follow mode it
// first emits the last nlines lines, then streams appended lines until ctx is
// cancelled, reopening the file when it is rotated (replaced) or truncated;
// waiting for new data uses inotify where available (see watcher_linux.go) and
// falls back to polling. In non-follow mode it reads the file once and returns
// at EOF. start >= 0 resumes reading from that byte offset in either mode.
func streamFile(ctx context.Context, path string, follow bool, nlines int, start int64, send func(string, int64)) {
	// Compressed archives are decoded transparently and always read whole, from
	// the start: they are rotation leftovers that will never grow, so following
	// them (or sampling their trailing bytes) is meaningless.
	if decoder(path) != nil {
		streamCompressed(ctx, path, send)
		return
	}

	var offset int64
	if start >= 0 {
		offset = start // resume: the client already holds everything before this
	} else if follow {
		lines, size := lastLines(path, nlines)
		for _, line := range lines {
			if ctx.Err() != nil {
				return
			}
			send(line, size) // the whole batch ends at size; that's the resume point
		}
		offset = size
	}

	var partial []byte
	var prev os.FileInfo
	caughtUp, notified := false, false

	var w *fileWatch
	if follow {
		w = watchFile(path)
		defer w.close()
	}
	buf := make([]byte, 1<<20) // the read-chunk buffer, shared across rounds

	// fail surfaces an open or read error once — a silently empty view is
	// undebuggable (missing file, permission denied) — and reports whether to
	// keep going: follow waits for the file to (re)appear, read-once gives up.
	fail := func(err error) bool {
		if !notified {
			notified = true
			send("tailon-ng: "+err.Error(), -1)
		}
		if !follow {
			return false
		}
		if !caughtUp {
			caughtUp = true
			send("", posCaughtUp) // nothing to catch up on: live immediately
		}
		return w.wait(ctx)
	}

	for {
		if ctx.Err() != nil {
			return
		}

		f, err := os.Open(path)
		if err != nil {
			if !fail(err) {
				return
			}
			continue
		}

		if info, err := f.Stat(); err == nil {
			if prev != nil && !os.SameFile(info, prev) {
				// Rotation: a different file lives at path now; start it fresh.
				offset = 0
				partial = nil
				if w != nil {
					w.rewatch() // the watch followed the old inode; re-attach by path
				}
			} else if info.Size() < offset {
				// Truncation. Served files are treated as append-only — the
				// resume/cache design builds on that — so a shrunk file is not
				// re-read: skip to its new end, keep following, leave a
				// warning. (copytruncate rotation lands here with size ~0, so
				// fresh lines still stream from the top.)
				slog.Warn("file shrank; skipping to its new end",
					"path", path, "size", info.Size(), "offset", offset)
				offset = info.Size()
				partial = nil
			}
			prev = info
		}

		// Read and emit in bounded chunks — never the whole file at once, so a
		// terabyte-sized view streams through one fixed buffer. The
		// unterminated tail is carried across chunks (emitLines' follow
		// behavior) and flushed after EOF when this is a read-once pass.
		if _, err = f.Seek(offset, io.SeekStart); err == nil {
			var n int
			for err == nil && ctx.Err() == nil {
				n, err = f.Read(buf)
				offset += int64(n)
				partial = emitLines(append(partial, buf[:n]...), offset, send)
			}
		}
		f.Close()
		if ctx.Err() != nil {
			return
		}
		if err != io.EOF {
			// A mid-read failure retries next round from the same offset (the
			// bytes read so far were emitted or are held in partial).
			if !fail(err) {
				return
			}
			continue
		}

		if !follow {
			if len(partial) > 0 {
				// The file's unterminated last line (a held trailing "\r" was
				// a line terminator after all, not half a CRLF pair).
				send(strings.TrimSuffix(string(partial), "\r"), offset)
			}
			return
		}
		if !caughtUp {
			caughtUp = true
			send("", posCaughtUp) // the backlog is rendered; what follows is live
		}
		if !w.wait(ctx) {
			return
		}
	}
}

// emitLines sends every complete line in data. offset is the absolute position
// of data's end, so each line's position — the resume point just past it — is
// offset minus what remains unconsumed. Lines end at "\n", "\r\n" or a lone
// "\r" (CR-only files exist; a file whose tail held no "\n" at all used to
// render as nothing). The unterminated tail — which later bytes may complete,
// or turn a trailing "\r" into a "\r\n" pair — is returned to carry into the
// next read; a read-once pass flushes it as a final line at EOF (streamFile).
func emitLines(data []byte, offset int64, send func(string, int64)) []byte {
	for {
		i := bytes.IndexAny(data, "\r\n")
		if i < 0 {
			return data // hold an unterminated line until its newline arrives
		}
		if data[i] == '\r' && i == len(data)-1 {
			return data // might be the first half of a CRLF pair; wait
		}
		line := string(data[:i])
		skip := 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			skip = 2
		}
		data = data[i+skip:]
		send(line, offset-int64(len(data)))
	}
}

// decoder returns a constructor for the decompressor matching the file's
// extension, or nil for plain files. gzip and bzip2 come from the standard
// library; xz and zstd are the project's only third-party dependencies.
func decoder(path string) func(io.Reader) (io.Reader, error) {
	switch {
	case strings.HasSuffix(path, ".gz"):
		return func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }
	case strings.HasSuffix(path, ".bz2"):
		return func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }
	case strings.HasSuffix(path, ".xz"):
		return func(r io.Reader) (io.Reader, error) { return xz.NewReader(r) }
	case strings.HasSuffix(path, ".zst"):
		return func(r io.Reader) (io.Reader, error) {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			// The zstd decoder runs goroutines that live until Close: hand the
			// callers a Closer so they can release them (see below).
			return zr.IOReadCloser(), nil
		}
	}
	return nil
}

// closeDecoder releases a decoder returned by decoder() if it holds resources
// (the zstd reader's goroutines; gzip is a no-op Close). Call it when done
// reading a decoded stream.
func closeDecoder(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		c.Close()
	}
}

// streamCompressed sends each line of a compressed archive, decoded. A line's
// pos is the number of *compressed* bytes consumed so far — measured against
// the on-disk size it drives the progress bar. It is not a resume offset:
// archives are immutable, the client caches them whole and never resumes
// mid-way (the stream handler knows the file is compressed and never forwards
// pos as an offset). Reading is line-buffered rather than whole-file, so large
// archives don't sit in memory.
func streamCompressed(ctx context.Context, path string, send func(string, int64)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var pos int64
	cr := &countingReader{r: f, n: &pos}
	r, err := decoder(path)(cr)
	if err != nil {
		send("tailon-ng: cannot decompress "+path+": "+err.Error(), -1)
		return
	}
	defer closeDecoder(r)

	br := bufio.NewReader(r)
	for ctx.Err() == nil {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" || err == nil {
			// Loaded atomically: the zstd decoder reads ahead from its own
			// goroutines, so the counter can advance concurrently.
			send(line, atomic.LoadInt64(&pos))
		}
		if err != nil {
			return
		}
	}
}

// countingReader counts bytes read through it into *n: the on-disk position of
// a possibly-decoded stream, which is what progress is measured in. The adds
// are atomic so a progress ticker may poll n from another goroutine (as
// /find's does) while the reader advances it.
type countingReader struct {
	r io.Reader
	n *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// sleep waits for d or until ctx is cancelled, returning false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// lastLines returns the final n lines of the file at path along with its size
// (the offset to follow from). It reads at most the trailing 256 KiB.
func lastLines(path string, n int) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0
	}
	size := info.Size()

	// The read window scales with n: tail's 10-line backlog needs little; the
	// view's full-scrollback backlog (MAX_LINES) far more — but bounded, so a
	// multi-gigabyte file is never read whole just to show its tail.
	maxRead := min(max(int64(n)*512, 256*1024), 16*1024*1024)
	start := max(size-maxRead, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, size
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, size
	}

	// Normalize CRLF and lone CR to LF, so Windows and CR-only files split
	// into lines too (a CR-only tail window used to come back empty).
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))

	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil, size
	}
	lines := strings.Split(trimmed, "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // we started mid-file, so the first line is partial
	}
	if n >= 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, size
}

// timeLayouts are the timestamp formats tried, in order, at (or near) the start
// of a log line when merging several files chronologically.
var timeLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00", // RFC3339 / ISO 8601, with timezone
	"2006-01-02T15:04:05.999999999",       // ISO 8601, no timezone
	"2006-01-02 15:04:05.999999999",       // ISO-ish, space separator
	"2006/01/02 15:04:05.999999999",       // slash-separated date
	"02/Jan/2006:15:04:05 -0700",          // Apache / common log format
	"Mon Jan 2 15:04:05 2006",             // Unix date / ctime
	"Jan 2 15:04:05",                      // syslog (RFC3164, no year)
}

// matchLayout tries to read a timestamp in the given layout from the start of
// line, skipping a leading syslog priority ("<13>") or "[" bracket. Only the
// leading whitespace-separated fields the layout needs are considered, so
// trailing log text is ignored.
func matchLayout(layout, line string) (time.Time, bool) {
	s := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(s, "<") {
		if i := strings.IndexByte(s, '>'); i > 0 && i <= 4 {
			s = s[i+1:]
		}
	}
	s = strings.TrimPrefix(s, "[")

	fields := strings.Fields(s)
	n := strings.Count(layout, " ") + 1
	if len(fields) < n {
		return time.Time{}, false
	}
	candidate := strings.TrimRight(strings.Join(fields[:n], " "), "]")
	if t, err := time.Parse(layout, candidate); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// timestamper extracts a timestamp from each line of a single file. The layout
// that matched last is tried first (a file rarely changes format mid-stream);
// when it fails, every layout is tried, so a file whose first timestamp comes
// late — or that switches format — is picked up without any locked-in
// detection. A line with no recognizable timestamp inherits the file's
// previous one, so multi-line entries stay together; before any is seen, lines
// fall back to time.Now — used for ordering only, never shown. Streams prime
// their stampers from the file itself (see primeTimestamper) so the backlog
// has a "previous one" to inherit even when it starts mid-entry.
type timestamper struct {
	layout string    // the layout that matched last, tried first
	last   time.Time // the last timestamp seen; inherited by undated lines
}

func (t *timestamper) stamp(line string) time.Time {
	if t.layout != "" {
		if ts, ok := matchLayout(t.layout, line); ok {
			t.last = ts
			return ts
		}
	}
	for _, layout := range timeLayouts {
		if layout == t.layout {
			continue // already tried
		}
		if ts, ok := matchLayout(layout, line); ok {
			t.layout, t.last = layout, ts
			return ts
		}
	}
	if !t.last.IsZero() {
		return t.last // continuation line: keep with the previous entry
	}
	return time.Now()
}

// primeStamperLines is how many trailing lines of a file are examined to prime
// its timestamper: enough history that a backlog ending in undated
// continuation lines (a stack trace, say) still finds its entry's date
// further back in the log.
const primeStamperLines = 200

// primeTimestamper returns a timestamper for path with the layout and the
// "previous date" already known: it stamps the file's trailing lines, stopping
// before the final skip lines (the backlog the stream is about to deliver,
// which stamps itself). A file with no timestamps in that window keeps the
// time.Now fallback.
func primeTimestamper(path string, skip int) *timestamper {
	t := &timestamper{}
	lines, _ := lastLines(path, primeStamperLines)
	for _, line := range lines[:max(0, len(lines)-skip)] {
		t.stamp(line)
	}
	// A file with no recognizable timestamp anywhere: anchor it at the file's
	// modification time — the moment its last line was written — so its
	// backlog sorts near its true age instead of as "now".
	if t.last.IsZero() {
		if fi, err := os.Stat(path); err == nil {
			t.last = fi.ModTime()
		}
	}
	return t
}

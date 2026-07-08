package main

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"io"
	"os"
	"strings"
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

	for {
		if ctx.Err() != nil {
			return
		}

		f, err := os.Open(path)
		if err != nil {
			// Surface the reason once — a silently empty view is undebuggable
			// (missing file, permission denied). Follow keeps waiting: files
			// that don't exist yet may appear.
			if !notified {
				notified = true
				send("tailon-ng: "+err.Error(), -1)
			}
			if !follow {
				return
			}
			if !caughtUp {
				caughtUp = true
				send("", posCaughtUp) // nothing to catch up on: live immediately
			}
			if !w.wait(ctx) {
				return
			}
			continue
		}

		// Detect rotation (a different file now lives at path) or truncation
		// (the file shrank) and restart from the beginning in either case.
		if info, err := f.Stat(); err == nil {
			if (prev != nil && !os.SameFile(info, prev)) || info.Size() < offset {
				offset = 0
				partial = nil
				if w != nil {
					w.rewatch() // the watch followed the old inode; re-attach by path
				}
			}
			prev = info
		}

		f.Seek(offset, io.SeekStart)
		data, _ := io.ReadAll(f)
		f.Close()
		offset += int64(len(data))

		// offset is now the absolute position of the end of data, so the
		// position just past each line is offset minus what remains unconsumed.
		// Lines end at "\n", "\r\n" or a lone "\r" (CR-only files exist; a file
		// whose tail held no "\n" at all used to render as nothing).
		data = append(partial, data...)
		partial = nil
		for {
			i := bytes.IndexAny(data, "\r\n")
			if i < 0 {
				if follow {
					partial = data // hold an unterminated line until its newline arrives
				} else if len(data) > 0 {
					send(string(data), offset)
				}
				break
			}
			if data[i] == '\r' && i == len(data)-1 && follow {
				partial = data // might be the first half of a CRLF pair; wait
				break
			}
			line := string(data[:i])
			skip := 1
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				skip = 2
			}
			data = data[i+skip:]
			send(line, offset-int64(len(data)))
		}

		if !follow {
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

	cr := &countingReader{r: f}
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
			send(line, cr.n)
		}
		if err != nil {
			return
		}
	}
}

// countingReader counts the bytes read through it: the compressed-side
// position of a decoded stream, which is what archive progress is measured in.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// sleep waits for d or until ctx is cancelled, returning false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
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

	const maxRead = 256 * 1024
	start := int64(0)
	if size > maxRead {
		start = size - maxRead
	}
	f.Seek(start, io.SeekStart)
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

// matchAny returns the time from the first layout that matches line.
func matchAny(line string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, ok := matchLayout(layout, line); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// detectLayout returns the layout matching the most of the sample lines, so the
// format is chosen from several lines rather than guessed from one. It returns
// "" if no layout matches any line.
func detectLayout(sample []string) string {
	best, bestN := "", 0
	for _, layout := range timeLayouts {
		n := 0
		for _, line := range sample {
			if _, ok := matchLayout(layout, line); ok {
				n++
			}
		}
		if n > bestN {
			best, bestN = layout, n
		}
	}
	return best
}

// detectSample is how many of a file's first lines are sampled to detect its
// timestamp format before that format is locked in.
const detectSample = 10

// timestamper extracts a timestamp from each line of a single file. It detects
// the file's format from its first detectSample lines (chosen across several
// lines rather than guessed from one) and then reuses it. A line with no
// recognizable timestamp inherits the file's previous one, so multi-line entries
// stay together; if the file has none, lines fall back to time.Now.
type timestamper struct {
	layout string
	sample []string
	last   time.Time
}

func (t *timestamper) stamp(line string) time.Time {
	var ts time.Time
	var ok bool
	switch t.layout {
	case "none": // no usable timestamp format in this file
	case "": // still sampling to detect the format
		ts, ok = matchAny(line)
		if t.sample = append(t.sample, line); len(t.sample) >= detectSample {
			if t.layout = detectLayout(t.sample); t.layout == "" {
				t.layout = "none"
			}
			t.sample = nil
		}
	default:
		ts, ok = matchLayout(t.layout, line)
	}
	switch {
	case ok:
		t.last = ts
		return ts
	case !t.last.IsZero():
		return t.last // continuation line: keep with the previous entry
	default:
		return time.Now()
	}
}

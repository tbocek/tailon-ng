package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"
)

// pollInterval is how often follow mode checks a file for newly appended data.
const pollInterval = 250 * time.Millisecond

// streamFile sends each line of the file at path to send. In follow mode it
// first emits the last nlines lines, then streams appended lines until ctx is
// cancelled, reopening the file when it is rotated (replaced) or truncated. In
// non-follow mode it reads the whole file once, from the start, and returns at
// EOF. This replaces shelling out to tail and grep.
func streamFile(ctx context.Context, path string, follow bool, nlines int, send func(string)) {
	var offset int64
	if follow {
		lines, size := lastLines(path, nlines)
		for _, line := range lines {
			if ctx.Err() != nil {
				return
			}
			send(line)
		}
		offset = size
	}

	var partial []byte
	var prev os.FileInfo

	for {
		if ctx.Err() != nil {
			return
		}

		f, err := os.Open(path)
		if err != nil {
			if !follow {
				return
			}
			if !sleep(ctx, pollInterval) {
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
			}
			prev = info
		}

		f.Seek(offset, io.SeekStart)
		data, _ := io.ReadAll(f)
		f.Close()
		offset += int64(len(data))

		data = append(partial, data...)
		partial = nil
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				if follow {
					partial = data // hold an unterminated line until its newline arrives
				} else if len(data) > 0 {
					send(strings.TrimRight(string(data), "\r"))
				}
				break
			}
			send(strings.TrimRight(string(data[:i]), "\r"))
			data = data[i+1:]
		}

		if !follow {
			return
		}
		if !sleep(ctx, pollInterval) {
			return
		}
	}
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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ListEntry is an entry that appears in the UI file input. createListing turns
// the configured sources into one ListEntry per served file, which the backend
// sends to the client.
type ListEntry struct {
	Path  string `json:"path"`
	Stale bool   `json:"stale"` // rotated/compressed: no longer written to
}

// staleRE matches the file-name shapes log rotation leaves behind: a compressed
// extension (.gz/.bz2/.xz/.zst), a numeric rotation suffix (.1, .2.gz), a date
// suffix (-20260702 or .20260702, optionally compressed), or .old/.bak. Such
// files are never written to again, so they are excluded from live tailing.
// The rotation counter is at most 3 digits and must not follow another digit,
// so "backup.2025" and per-host files like "192.168.1.5" stay live.
var staleRE = regexp.MustCompile(`(?i)(\.(gz|bz2|xz|zst|old|bak)|[.-]\d{8}|[^.\d]\.\d{1,3})$`)

func isStale(path string) bool { return staleRE.MatchString(path) }

// allFiles is the set of files currently served. It is guarded by allFilesMu
// because createListing rebuilds it (on every /list request) while the stream
// and download handlers read it concurrently.
var (
	allFilesMu sync.RWMutex
	allFiles   = map[string]bool{}
)

// createListing rebuilds the served-file set from the configured sources and
// returns it as a flat, path-sorted listing for the UI. Each source is a file,
// a directory (walked recursively, so files in subdirectories — and ones created
// later — are served too), or a shell glob. In a glob, "*" matches within one
// directory and "**" across directories, so "/var/log/**.log" finds .log files
// at any depth.
func createListing(sources []string) []*ListEntry {
	files := make(map[string]bool)
	for _, src := range sources {
		switch {
		case strings.Contains(src, "**"):
			for _, m := range globStar(src) { // recursive glob; matches files only
				files[m] = true
			}
		case strings.ContainsAny(src, "*?["):
			matches, _ := filepath.Glob(src) // single-level glob; can match a directory
			for _, m := range matches {
				addTree(files, m)
			}
		default:
			addTree(files, src)
		}
	}

	res := make([]*ListEntry, 0, len(files))
	for p := range files {
		if isBinary(p) {
			delete(files, p) // drop from the allowlist too: not served at all
			continue
		}
		res = append(res, &ListEntry{Path: p, Stale: isStale(p)})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Path < res[j].Path })

	allFilesMu.Lock()
	allFiles = files
	allFilesMu.Unlock()
	return res
}

// isBinary reports whether the file's first kilobyte contains a NUL byte —
// the classic binary heuristic (git and grep use the same): text in any
// log-plausible encoding never contains NUL, while ELF binaries, wtmp
// databases, journald files and the like show one within the first bytes.
// Binaries are dropped from the listing: they render as garbage and, without
// newlines to split on, would buffer without bound. Compressed archives are
// exempt — binary by nature, decoded transparently — and so is anything not
// a plain readable file: a not-yet-existing path stays servable (judged again
// on the next listing), and non-regular files are dropped without opening
// them (reading a FIFO would block).
func isBinary(path string) bool {
	if decoder(path) != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !info.Mode().IsRegular() {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 1024)
	n, _ := f.Read(buf)
	return bytes.IndexByte(buf[:n], 0) >= 0
}

// addTree adds one served path to files: a directory is walked recursively,
// anything else is a single file (which need not exist yet).
func addTree(files map[string]bool, p string) {
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		files[p] = true
		return
	}
	// WalkDir lstats its root, so a symlinked directory would be treated as a
	// plain file and then dropped; a trailing separator makes the OS resolve
	// the link, and "tailon-ng /var/log/current" (current -> dir) serves the
	// tree. Symlinks below the root are still not followed (no cycle risk).
	if !strings.HasSuffix(p, string(filepath.Separator)) {
		p += string(filepath.Separator)
	}
	filepath.WalkDir(p, func(q string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files[q] = true
		}
		return nil
	})
}

// globStar expands a glob containing "**" into matching file paths. filepath.Glob
// does not understand "**", so we walk from the pattern's fixed prefix and
// regexp-match every file beneath it.
func globStar(pattern string) []string {
	re := globRegexp(pattern)
	var matches []string
	filepath.WalkDir(globBase(pattern), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && re.MatchString(p) {
			matches = append(matches, p)
		}
		return nil
	})
	return matches
}

// globBase returns the longest leading directory of pattern that contains no
// glob metacharacters — where globStar starts walking. It is "." when the first
// path segment is already a pattern.
func globBase(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return pattern
	}
	if j := strings.LastIndex(pattern[:i], "/"); j >= 0 {
		return pattern[:j+1]
	}
	return "."
}

// globRegexp compiles a glob into an anchored regexp: "*" matches any run of
// non-separator characters, "**" matches anything (crossing "/"), "**/" also
// spans zero directories, "?" matches one non-separator, and "[...]" is a class.
func globRegexp(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?") // "**/" spans zero or more directories
					i += 2
				} else {
					b.WriteString(".*") // "**" spans any characters, including "/"
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			if j := strings.IndexByte(pattern[i:], ']'); j > 1 {
				class := pattern[i+1 : i+j]
				if rest, ok := strings.CutPrefix(class, "!"); ok {
					class = "^" + rest
				}
				b.WriteByte('[')
				b.WriteString(class)
				b.WriteByte(']')
				i += j
			} else {
				b.WriteString(regexp.QuoteMeta(string(c)))
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	if re, err := regexp.Compile(b.String()); err == nil {
		return re
	}
	return regexp.MustCompile(`\b\B`) // unparseable class: match nothing
}

func fileAllowed(path string) bool {
	allFilesMu.RLock()
	defer allFilesMu.RUnlock()
	_, ok := allFiles[path]
	return ok
}

// allowedFiles returns the sorted served files whose path starts with prefix.
// "" selects everything (the "all files" mode); a directory (the client sends
// "dir/") or any path prefix such as ".../192.168.1" scopes to a group of
// hosts, or a file together with its rotated archives. Filtering the existing
// allowlist keeps it safe — no path outside the served set can match.
func allowedFiles(prefix string) []string {
	allFilesMu.RLock()
	names := make([]string, 0, len(allFiles))
	for p := range allFiles {
		if strings.HasPrefix(p, prefix) {
			names = append(names, p)
		}
	}
	allFilesMu.RUnlock()
	sort.Strings(names)
	return names
}

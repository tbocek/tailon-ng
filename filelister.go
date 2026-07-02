package main

import (
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
var staleRE = regexp.MustCompile(`(?i)(\.(gz|bz2|xz|zst)|\.\d+|[.-]\d{8}|\.(old|bak))$`)

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
	var res []*ListEntry
	add := func(p string) {
		if files[p] {
			return
		}
		files[p] = true
		res = append(res, &ListEntry{Path: p, Stale: isStale(p)})
	}
	// addPath serves one path: a directory is walked recursively, anything else
	// is added as a single file (which need not exist yet).
	addPath := func(p string) {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			filepath.WalkDir(p, func(q string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					add(q)
				}
				return nil
			})
		} else {
			add(p)
		}
	}

	for _, src := range sources {
		switch {
		case strings.Contains(src, "**"):
			for _, m := range globStar(src) { // recursive glob; matches files only
				add(m)
			}
		case strings.ContainsAny(src, "*?["):
			matches, _ := filepath.Glob(src) // single-level glob
			for _, m := range matches {
				addPath(m)
			}
		default:
			addPath(src)
		}
	}

	sort.Slice(res, func(i, j int) bool { return res[i].Path < res[j].Path })

	allFilesMu.Lock()
	allFiles = files
	allFilesMu.Unlock()
	return res
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
				b.WriteString("[" + class + "]")
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

// allowedFiles returns the sorted list of all served files. It backs the "all
// files" stream mode, which tails every file at once.
func allowedFiles() []string {
	allFilesMu.RLock()
	names := make([]string, 0, len(allFiles))
	for p := range allFiles {
		names = append(names, p)
	}
	allFilesMu.RUnlock()
	sort.Strings(names)
	return names
}

// allowedUnder returns the served files beneath the directory prefix,
// recursively. It backs the per-subfolder stream, which tails or greps just the
// logs under one directory. Filtering the existing allowlist keeps it safe — no
// path outside the served set can match.
func allowedUnder(prefix string) []string {
	var sel []string
	for _, p := range allowedFiles() {
		if strings.HasPrefix(p, prefix+"/") {
			sel = append(sel, p)
		}
	}
	return sel
}

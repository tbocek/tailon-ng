package main

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ListEntry is an entry that appears in the UI file input.
// FileSpecs are transformed into one or more ListEntry instances, which the backend sends to the client.
type ListEntry struct {
	Path    string    `json:"path"`
	Alias   string    `json:"alias"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	Exists  bool      `json:"exists"`
}

func fileInfo(path string) *ListEntry {
	entry := ListEntry{}
	entry.Path = path

	info, err := os.Stat(path)
	if !os.IsNotExist(err) {
		entry.Exists = true
		entry.Size = info.Size()
		entry.ModTime = info.ModTime()
	}

	return &entry
}

// allFiles is the set of files currently served. It is guarded by allFilesMu
// because createListing rebuilds it (on every /list request) while the stream
// and download handlers read it concurrently.
var (
	allFilesMu sync.RWMutex
	allFiles   = map[string]bool{}
)

func createListing(filespecs []FileSpec) map[string][]*ListEntry {
	files := make(map[string]bool)
	res := make(map[string][]*ListEntry)

	for _, spec := range filespecs {
		group := "__default__"
		if spec.Group != "" {
			group = spec.Group
		}

		switch spec.Type {
		case "file":
			entry := fileInfo(spec.Path)
			if spec.Alias != "" {
				entry.Alias = spec.Alias
			} else {
				entry.Alias = entry.Path
			}
			res[group] = append(res[group], entry)
			files[entry.Path] = true
		case "glob":
			matches, _ := filepath.Glob(spec.Path)
			for _, match := range matches {
				entry := fileInfo(match)
				if spec.Alias != "" {
					entry.Alias = path.Join(spec.Alias, path.Base(entry.Path))
				} else {
					cwd, _ := os.Getwd()
					rel, _ := filepath.Rel(cwd, entry.Path)
					entry.Alias = rel
				}
				res[group] = append(res[group], entry)
				files[entry.Path] = true
			}
		case "dir":
			// Serve every file under the directory, recursively.
			filepath.WalkDir(spec.Path, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				entry := fileInfo(p)
				if spec.Alias != "" {
					entry.Alias = path.Join(spec.Alias, path.Base(entry.Path))
				} else {
					cwd, _ := os.Getwd()
					rel, _ := filepath.Rel(cwd, entry.Path)
					entry.Alias = rel
				}
				res[group] = append(res[group], entry)
				files[entry.Path] = true
				return nil
			})
		}
	}

	allFilesMu.Lock()
	allFiles = files
	allFilesMu.Unlock()
	return res
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

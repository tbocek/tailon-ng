//go:build !unix

package main

import "os"

// fileID is unavailable off unix; resume falls back to the size-only check.
func fileID(fi os.FileInfo) string { return "" }

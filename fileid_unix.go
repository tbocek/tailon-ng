//go:build unix

package main

import (
	"os"
	"strconv"
	"syscall"
)

// fileID returns a stable identity for the file behind fi ("dev-inode"), or
// "" when unavailable. A resuming client echoes it, so a cached byte offset is
// only ever applied to the very file it was read from — after rotation a
// same-named file may already have grown past the offset, and size alone
// cannot tell (see streamHandler's resume check).
func fileID(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + "-" + strconv.FormatUint(uint64(st.Ino), 10)
}

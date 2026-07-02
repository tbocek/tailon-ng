package main

import (
	"embed"
	"io/fs"
)

// frontendDist holds the frontend assets (Go templates, CSS and JS — four flat
// files), embedded into the binary at build time. The frontend is plain
// hand-written HTML/CSS/JS under ./frontend — there is no build step or
// toolchain.
//
//go:embed frontend
var frontendDist embed.FS

// frontendAssets is frontendDist rooted at the "frontend" directory, so the
// files are addressed by bare name ("main.css", "base.html"), matching the
// /vfs/ URLs and template paths the server uses.
var frontendAssets = mustSub(frontendDist, "frontend")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

package main

import (
	"embed"
	"io/fs"
)

// frontendDist holds the frontend assets (HTML templates and static files),
// embedded into the binary at build time. The frontend is plain hand-written
// HTML/CSS/JS under ./frontend — there is no build step or toolchain.
//
//go:embed frontend/dist frontend/templates
var frontendDist embed.FS

// frontendAssets is frontendDist rooted at the "frontend" directory, so asset
// paths are "dist/..." and "templates/...", matching the URLs and template
// paths the server uses.
var frontendAssets = mustSub(frontendDist, "frontend")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

package main

import _ "embed"

// The frontend is plain hand-written HTML/CSS/JS under ./frontend — three flat
// files, no build step or toolchain — each embedded into the binary directly.
// main.html is a Go template rendered server-side (see indexHandler); the CSS
// and JS are served verbatim (see setupRoutes).

//go:embed frontend/main.html
var indexHTML string

//go:embed frontend/main.css
var mainCSS []byte

//go:embed frontend/main.js
var mainJS []byte

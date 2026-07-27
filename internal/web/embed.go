package web

import "embed"

// dist is the built single-page app. It is committed so `go build` and
// `go install` alone produce a working binary — the placeholder index.html is
// replaced by `make ui` (vite writes straight into this directory).
//
//go:embed all:dist
var dist embed.FS

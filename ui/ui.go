// Package ui embeds the management homepage's static assets (index.html and
// app.js) so the plans binary is fully self-contained — no runtime file
// dependencies, no build step. See server/ui.go for how these get wired into
// the HTTP routers.
package ui

import "embed"

// Files holds ui/index.html and ui/app.js, servable directly via
// http.FileServer(http.FS(Files)) — "/" resolves to index.html, "/app.js"
// resolves to app.js.
//
//go:embed index.html app.js
var Files embed.FS

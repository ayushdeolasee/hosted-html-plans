package server

import (
	"net/http"

	uiassets "github.com/ayushdeolasee/hosted-html-plans/ui"
)

// uiFileServer serves the embedded UI assets (ui/index.html, ui/app.js) from
// the ui package's embed.FS. Requesting "/" resolves to index.html the same
// way http.FileServer resolves any directory index.
var uiFileServer = http.FileServer(http.FS(uiassets.Files))

// ServeUI renders the embedded management homepage (ui/index.html) and its
// static asset (ui/app.js). It's wired in as Server.HomepageHandler's
// replacement and mounted at GET /app.js — see FullHandler and NewServer in
// http.go.
func ServeUI(w http.ResponseWriter, r *http.Request) {
	uiFileServer.ServeHTTP(w, r)
}

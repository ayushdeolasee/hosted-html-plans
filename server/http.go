package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FunnelController abstracts the on-demand public funnel listener. The tsnet
// phase provides a real implementation; share handlers call these hooks.
type FunnelController interface {
	// EnsureOpen opens the funnel listener if it isn't already open.
	EnsureOpen() error
	// CloseIfIdle closes the funnel listener if no active shares remain.
	CloseIfIdle() error
}

// NoopFunnel is the phase-1 stand-in: sharing works, but no funnel is opened.
type NoopFunnel struct{}

func (NoopFunnel) EnsureOpen() error  { return nil }
func (NoopFunnel) CloseIfIdle() error { return nil }

// Server wires the store, config, and funnel into HTTP handlers.
type Server struct {
	Store  *Store
	Funnel FunnelController
	Logger *log.Logger

	// Config is the live, in-memory configuration. It can be mutated at
	// runtime by the settings API (PUT /api/settings), so all access goes
	// through cfgMu; use config()/setConfig() rather than touching it
	// directly. Exported for construction convenience only.
	Config Config
	cfgMu  sync.RWMutex

	// tailnet is the runtime Tailscale manager (start/stop/status). It may be
	// nil in tests that don't exercise Tailscale.
	tailnet *TailnetManager

	// restartRequired is set when a config change needs a process restart to
	// take effect (currently only lan_listen changes). Surfaced by the
	// settings API and sticky until the process restarts.
	restartRequired atomic.Bool

	// HomepageHandler renders GET /. A later phase swaps in the embedded UI
	// by replacing this field before FullHandler is called.
	HomepageHandler http.HandlerFunc

	// tnNode / tnDomain hold the live tailnet identity, filled in by the
	// tsnet phase once the node is authenticated (see SetTailnetIdentity).
	// They override Config.Hostname / Config.TailnetDomain for URL building
	// so absolute https URLs appear as soon as the tailnet comes up, without
	// rebuilding the Server. Read at request time; safe for concurrent use.
	tnNode   atomic.Pointer[string]
	tnDomain atomic.Pointer[string]
}

// config returns a snapshot copy of the live config, safe for concurrent use.
func (s *Server) config() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.Config
}

// setConfig replaces the live config. Safe for concurrent use.
func (s *Server) setConfig(c Config) {
	s.cfgMu.Lock()
	s.Config = c
	s.cfgMu.Unlock()
}

// SetTailnetManager wires the runtime Tailscale manager into the server so the
// settings API can start/stop the tailnet live.
func (s *Server) SetTailnetManager(m *TailnetManager) { s.tailnet = m }

// SetTailnetIdentity records the live tailnet node label and domain suffix
// (e.g. "plans", "tailnet-name.ts.net") once tsnet is authenticated. After
// this call, planURLs/shareURL emit absolute https URLs. Concurrency-safe.
func (s *Server) SetTailnetIdentity(node, domain string) {
	if node != "" {
		s.tnNode.Store(&node)
	}
	if domain != "" {
		s.tnDomain.Store(&domain)
	}
}

// ClearTailnetIdentity forgets the live tailnet identity so plan/share URLs
// revert to relative form. Called when the tailnet is stopped at runtime.
func (s *Server) ClearTailnetIdentity() {
	empty := ""
	s.tnNode.Store(&empty)
	s.tnDomain.Store(&empty)
}

// tnHost returns the live tailnet node label, falling back to config.
func (s *Server) tnHost() string {
	if p := s.tnNode.Load(); p != nil && *p != "" {
		return *p
	}
	return s.config().Hostname
}

// tnSuffix returns the live tailnet domain suffix, falling back to config.
func (s *Server) tnSuffix() string {
	if p := s.tnDomain.Load(); p != nil && *p != "" {
		return *p
	}
	return s.config().TailnetDomain
}

// NewServer builds a Server with sensible defaults.
func NewServer(store *Store, cfg Config, funnel FunnelController, logger *log.Logger) *Server {
	if funnel == nil {
		funnel = NoopFunnel{}
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{Store: store, Config: cfg, Funnel: funnel, Logger: logger}
	s.HomepageHandler = ServeUI
	return s
}

// FullHandler returns the router for the private (LAN + tailnet) listeners.
func (s *Server) FullHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { s.HomepageHandler(w, r) })
	mux.HandleFunc("GET /app.js", ServeUI)

	mux.HandleFunc("POST /api/plans", s.handleCreate)
	mux.HandleFunc("GET /api/plans", s.handleList)
	mux.HandleFunc("GET /api/plans/{slug}", s.handleGetPlan)
	mux.HandleFunc("PUT /api/plans/{slug}", s.handleRevise)
	mux.HandleFunc("PATCH /api/plans/{slug}", s.handleRename)
	mux.HandleFunc("DELETE /api/plans/{slug}", s.handleDelete)
	mux.HandleFunc("POST /api/plans/{slug}/append", s.handleAppend)
	mux.HandleFunc("POST /api/plans/{slug}/status", s.handleStatus)
	mux.HandleFunc("POST /api/plans/{slug}/restore", s.handleRestore)
	mux.HandleFunc("GET /api/plans/{slug}/history", s.handleHistory)
	mux.HandleFunc("DELETE /api/plans/{slug}/history", s.handlePruneHistory)
	mux.HandleFunc("DELETE /api/plans/{slug}/history/{n}", s.handleDeleteVersion)
	mux.HandleFunc("GET /api/plans/{slug}/events", s.handleEvents)
	mux.HandleFunc("POST /api/plans/{slug}/share", s.handleShare)
	mux.HandleFunc("DELETE /api/plans/{slug}/share", s.handleUnshare)

	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)
	mux.HandleFunc("GET /api/tailnet/status", s.handleTailnetStatus)

	mux.HandleFunc("GET /api/trash", s.handleListTrash)
	mux.HandleFunc("POST /api/trash/{slug}/restore", s.handleRestoreTrash)
	mux.HandleFunc("DELETE /api/trash/{slug}", s.handlePurgeTrash)

	mux.HandleFunc("GET /p/{slug}", s.handleViewPlan)
	mux.HandleFunc("GET /share/{token}", s.handleShareView)

	return s.logging(mux)
}

// SharesHandler returns the shares-only router. This is what the funnel
// listener serves: exactly one route, GET /share/{token}. The tsnet phase
// binds it to the funnel listener.
func (s *Server) SharesHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /share/{token}", s.handleShareView)
	return s.logging(mux)
}

// ---- middleware ----

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer. Without this the SSE handler's
// type assertion to http.Flusher fails — statusRecorder would satisfy only
// http.ResponseWriter — and events would sit in the buffer indefinitely.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.Logger.Printf("%s %s %d %s", r.Method, r.URL.RequestURI(), rec.status, time.Since(start).Round(time.Microsecond))
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

const maxBody = 60 << 20 // headroom over the 50MB per-plan cap

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	return body, true
}

func writeParamsFrom(r *http.Request) WriteParams {
	q := r.URL.Query()
	return WriteParams{
		Slug:   q.Get("slug"),
		Title:  q.Get("title"),
		Agent:  q.Get("agent"),
		Repo:   q.Get("repo"),
		Branch: q.Get("branch"),
		Note:   q.Get("note"),
	}
}

// planURLs builds the ready-to-open LAN and tailnet URLs for a slug.
func (s *Server) planURLs(r *http.Request, slug string) map[string]string {
	urls := map[string]string{"lan": "http://" + r.Host + "/p/" + slug}
	if suffix := s.tnSuffix(); suffix != "" {
		urls["tailnet"] = fmt.Sprintf("https://%s.%s/p/%s", s.tnHost(), suffix, slug)
	} else {
		urls["tailnet"] = ""
	}
	return urls
}

// shareURL builds the public share URL shape.
func (s *Server) shareURL(token string) string {
	if suffix := s.tnSuffix(); suffix != "" {
		return fmt.Sprintf("https://%s.%s:8443/share/%s", s.tnHost(), suffix, token)
	}
	return "/share/" + token
}

func statusForStoreErr(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, ErrLatestVersion):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// ---- handlers ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	pl, version, err := s.Store.Create(body, writeParamsFrom(r))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      pl.ID,
		"slug":    pl.Slug,
		"version": version,
		"urls":    s.planURLs(r, pl.Slug),
	})
}

func (s *Server) handleRevise(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	base := -1 // omitted => forced
	if bv := r.URL.Query().Get("base_version"); bv != "" {
		n, err := strconv.Atoi(bv)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid base_version")
			return
		}
		base = n
	}
	pl, version, err := s.Store.Revise(slug, body, base, writeParamsFrom(r))
	if err != nil {
		var conflict ErrConflict
		if errors.As(err, &conflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "version conflict",
				"current": conflict.Current,
			})
			return
		}
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": pl.ID, "slug": pl.Slug, "version": version, "urls": s.planURLs(r, pl.Slug)})
}

func (s *Server) handleAppend(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	pl, version, err := s.Store.Append(slug, body, writeParamsFrom(r))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": pl.ID, "slug": pl.Slug, "version": version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var partial map[string]any
	if err := json.Unmarshal(body, &partial); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}
	pl, version, err := s.Store.Status(slug, partial, writeParamsFrom(r))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": pl.ID, "slug": pl.Slug, "version": version, "meta": pl.Meta})
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	n, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || n <= 0 {
		writeErr(w, http.StatusBadRequest, "version query param required")
		return
	}
	pl, version, err := s.Store.Restore(slug, n, writeParamsFrom(r))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": pl.ID, "slug": pl.Slug, "version": version})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	plans := s.Store.List(ListFilter{
		Repo: q.Get("repo"), Branch: q.Get("branch"), Status: q.Get("status"), Q: q.Get("q"),
	})
	if plans == nil {
		plans = []*Plan{}
	}
	writeJSON(w, http.StatusOK, plans)
}

func (s *Server) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	pl, err := s.Store.GetPlan(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": pl.ID, "slug": pl.Slug, "title": pl.Title,
		"created": pl.Created, "updated": pl.Updated,
		"latest": pl.Latest, "agent": pl.Agent,
		"repo": pl.Repo, "branch": pl.Branch, "status": pl.Status,
		"meta": pl.Meta, "shared": pl.ShareToken != nil,
		"history": pl.History,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	h, err := s.Store.History(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	if h == nil {
		h = []VersionEntry{}
	}
	writeJSON(w, http.StatusOK, h)
}

// handleDeleteVersion serves DELETE /api/plans/{slug}/history/{n}: a hard
// delete of one historical version (blob + index entry). The latest version
// is refused with 409 — deleting the plan itself is DELETE /api/plans/{slug}.
func (s *Server) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid version")
		return
	}
	pl, err := s.Store.DeleteVersion(slug, n)
	if err != nil {
		if errors.Is(err, ErrLatestVersion) {
			writeErr(w, http.StatusConflict,
				fmt.Sprintf("v%d is the latest version and cannot be deleted; delete the plan instead", n))
			return
		}
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": true, "version": n, "latest": pl.Latest, "history": pl.History,
	})
}

// handlePruneHistory serves DELETE /api/plans/{slug}/history: hard-delete all
// but the latest version. ?keep=N retains the N most recent versions instead
// (N >= 1; the latest always survives).
func (s *Server) handlePruneHistory(w http.ResponseWriter, r *http.Request) {
	keep := 1
	if v := r.URL.Query().Get("keep"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "keep must be an integer >= 1")
			return
		}
		keep = n
	}
	pl, removed, err := s.Store.PruneHistory(r.PathValue("slug"), keep)
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	if removed == nil {
		removed = []int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pruned": len(removed), "versions": removed, "kept": keep,
		"latest": pl.Latest, "history": pl.History,
	})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("slug")
	var req struct {
		Title string `json:"title"`
		Slug  string `json:"slug"`
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "body must be a JSON object")
			return
		}
	}
	// Query params are an accepted alternative.
	if req.Title == "" {
		req.Title = r.URL.Query().Get("title")
	}
	if req.Slug == "" {
		req.Slug = r.URL.Query().Get("slug")
	}
	pl, err := s.Store.Rename(key, req.Title, req.Slug)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": pl.ID, "slug": pl.Slug, "title": pl.Title})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	wasShared, err := s.Store.Delete(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	if wasShared {
		_ = s.Funnel.CloseIfIdle()
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleListTrash(w http.ResponseWriter, r *http.Request) {
	plans := s.Store.ListTrash()
	if plans == nil {
		plans = []*Plan{}
	}
	writeJSON(w, http.StatusOK, plans)
}

func (s *Server) handleRestoreTrash(w http.ResponseWriter, r *http.Request) {
	pl, err := s.Store.RestoreTrash(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "slug": pl.Slug})
}

func (s *Server) handlePurgeTrash(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.PurgeTrash(r.PathValue("slug")); err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged": true})
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	token, _, err := s.Store.Share(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	// Open the funnel listener if needed. The share itself already succeeded
	// (token stored); a funnel error is surfaced as a non-fatal warning so
	// the caller learns the public URL isn't live yet (tailnet not connected,
	// or the "funnel" node attribute is missing from the tailnet ACL).
	resp := map[string]any{"token": token, "url": s.shareURL(token)}
	if ferr := s.Funnel.EnsureOpen(); ferr != nil {
		s.Logger.Printf("funnel ensure-open: %v", ferr)
		resp["warning"] = ferr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleUnshare(w http.ResponseWriter, r *http.Request) {
	_, err := s.Store.Unshare(r.PathValue("slug"))
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	if ferr := s.Funnel.CloseIfIdle(); ferr != nil {
		s.Logger.Printf("funnel close-if-idle: %v", ferr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

// handleViewPlan serves /p/{slug}: latest, or ?version=N with a banner,
// and ?format=text for the style-stripped view.
func (s *Server) handleViewPlan(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	q := r.URL.Query()

	requested := 0
	if v := q.Get("version"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid version")
			return
		}
		requested = n
	}

	pl, body, err := s.Store.GetVersion(slug, requested)
	if err != nil {
		// Follow a slug redirect if one exists.
		if errors.Is(err, ErrNotFound) {
			if ns, ok := s.Store.Redirect(slug); ok {
				target := "/p/" + ns
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusMovedPermanently)
				return
			}
		}
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}

	if q.Get("format") == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, ToText(body))
		return
	}

	// format=raw serves the stored bytes verbatim — no banner, no live-reload
	// client. This is what `push-plan pull` fetches: whatever comes back here
	// is pushed straight back to the server on the next revision, so anything
	// injected would be round-tripped into the plan itself.
	if q.Get("format") == "raw" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(body)
		return
	}

	// Inject the historical-version banner when viewing a non-latest version.
	// Latest views instead get the live-reload client: a frozen historical
	// version has nothing to stream, and pinning it is the whole point.
	if requested != 0 && requested != pl.Latest {
		body = injectBanner(body, slug, requested, pl.Latest)
	} else {
		body = injectLive(body, slug, pl.Latest)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

// handleShareView serves a shared plan's LATEST version, read-only. Used by
// both the private router and the shares-only (funnel) router.
func (s *Server) handleShareView(w http.ResponseWriter, r *http.Request) {
	pl, err := s.Store.FindByShareToken(r.PathValue("token"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	_, body, err := s.Store.GetVersion(pl.Slug, 0)
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}
	// Block external network calls on public share responses (plan.html §11).
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'unsafe-inline' 'self'; style-src 'unsafe-inline' 'self'; "+
			"img-src 'self' data:; font-src 'self' data:; connect-src 'none'; frame-src 'none'; "+
			"object-src 'none'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

// injectBanner inserts a slim "Viewing vN of M" banner after <body>.
func injectBanner(body []byte, slug string, version, latest int) []byte {
	banner := fmt.Sprintf(
		`<div style="position:sticky;top:0;z-index:99999;background:#b45309;color:#fff;`+
			`font:600 14px/1.4 ui-sans-serif,-apple-system,sans-serif;padding:8px 16px;text-align:center">`+
			`Viewing v%d of %d · <a href="/p/%s" style="color:#fff;text-decoration:underline">jump to latest</a></div>`,
		version, latest, html.EscapeString(slug))
	lower := strings.ToLower(string(body))
	if i := strings.Index(lower, "<body"); i >= 0 {
		if gt := strings.Index(lower[i:], ">"); gt >= 0 {
			pos := i + gt + 1
			out := make([]byte, 0, len(body)+len(banner))
			out = append(out, body[:pos]...)
			out = append(out, banner...)
			out = append(out, body[pos:]...)
			return out
		}
	}
	return append([]byte(banner), body...)
}

// defaultHomepage is the placeholder homepage; a later phase replaces it via
// Server.HomepageHandler.
func (s *Server) defaultHomepage(w http.ResponseWriter, r *http.Request) {
	plans := s.Store.List(ListFilter{})
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1.0">`)
	b.WriteString(`<title>Plans</title><style>body{font-family:ui-sans-serif,-apple-system,sans-serif;max-width:760px;margin:40px auto;padding:0 20px;color-scheme:light dark}a{color:#2563eb}li{margin:6px 0}</style>`)
	b.WriteString(`</head><body><h1>Plans</h1>`)
	if len(plans) == 0 {
		b.WriteString(`<p>No plans yet. Push one with <code>POST /api/plans</code>.</p>`)
	} else {
		b.WriteString("<ul>")
		for _, pl := range plans {
			fmt.Fprintf(&b, `<li><a href="/p/%s">%s</a> <small>v%d · %s</small></li>`,
				html.EscapeString(pl.Slug), html.EscapeString(pl.Title), pl.Latest, html.EscapeString(pl.Slug))
		}
		b.WriteString("</ul>")
	}
	b.WriteString(`</body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

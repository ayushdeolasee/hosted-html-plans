package server

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// heartbeatInterval bounds how long an idle SSE connection stays silent.
// Tailscale funnel and most proxies drop connections with no traffic; a
// comment line keeps them open without waking the browser.
const heartbeatInterval = 25 * time.Second

// Hub broadcasts "plan {slug} is now at version N" to connected viewers.
//
// Subscriber channels are buffered with capacity 1 and carry latest-wins
// semantics: a subscriber that hasn't drained its channel gets the stale
// version replaced rather than the new one dropped. That is safe because a
// version number is not a delta — a viewer that misses v6 and only sees v7
// still ends up rendering the right document.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan int]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan int]struct{}{}}
}

// Subscribe returns a channel that receives the latest version of slug on
// every commit. The caller must Unsubscribe when it is done.
func (h *Hub) Subscribe(slug string) chan int {
	ch := make(chan int, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[slug] == nil {
		h.subs[slug] = map[chan int]struct{}{}
	}
	h.subs[slug][ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(slug string, ch chan int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.subs[slug]
	if m == nil {
		return
	}
	delete(m, ch)
	if len(m) == 0 {
		delete(h.subs, slug)
	}
	close(ch)
}

// Publish fans a new version out to every subscriber of slug. It never
// blocks: a full channel has its stale value swapped for the new one.
func (h *Hub) Publish(slug string, version int) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[slug] {
		select {
		case ch <- version:
		default:
			// Subscriber is behind: drop the value it hasn't read and
			// replace it with the newer one. Holding h.mu makes this the
			// only sender, so the second send cannot block on a refill.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- version:
			default:
			}
		}
	}
}

// Count reports the number of live subscribers for slug.
func (h *Hub) Count(slug string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[slug])
}

// handleEvents serves GET /api/plans/{slug}/events: an SSE stream that emits
// {"latest":N} whenever the plan gains a version.
//
// ?since=N lets a reconnecting (or slow-to-open) client state what it already
// rendered; if the plan has moved past N we send the current version straight
// away rather than making the viewer wait for the next write.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	pl, err := s.Store.GetPlan(slug)
	if err != nil {
		writeErr(w, statusForStoreErr(err), err.Error())
		return
	}

	since, _ := strconv.Atoi(r.URL.Query().Get("since"))

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat response buffering in any proxy sitting in front of us; without
	// this an intermediary may hold events until the stream closes.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.Store.Events.Subscribe(slug)
	defer s.Store.Events.Unsubscribe(slug, ch)

	send := func(version int) bool {
		if _, err := fmt.Fprintf(w, "data: {\"latest\":%d}\n\n", version); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Catch up a client that missed writes landing between page render and
	// stream open.
	if pl.Latest > since && !send(pl.Latest) {
		return
	}

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case v, ok := <-ch:
			if !ok || !send(v) {
				return
			}
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// injectLive splices the live-reload client into a served plan, just before
// </body>. Only latest-version views get it: history views are frozen by
// definition, and the text view has no place to put a script.
func injectLive(body []byte, slug string, version int) []byte {
	// The slug lands inside a JS string literal, and slugs are already
	// restricted to [a-z0-9-] (slugStrip), so %q is the right quoting here —
	// HTML-escaping would be the wrong escaper for this context.
	script := fmt.Sprintf(liveScript, slug, version)
	return insertBefore(body, []byte(script), "</body>", "</html>")
}

// liveScript is the injected client. Two %-verbs: slug, current version.
//
// On each event it refetches the plan and swaps <body> in place, preserving
// scroll position — a plan being written live is usually being read somewhere
// in the middle, and location.reload() would throw that away. If <head>
// changed (the document's inline CSS was revised) a body swap would leave
// stale styles, so we fall back to a real reload.
const liveScript = `<script data-plans-live>
(function () {
  if (window.__plansLive) return;
  window.__plansLive = true;
  var slug = %q, cur = %d;

  function toast(msg) {
    var t = document.createElement("div");
    t.textContent = msg;
    t.setAttribute("style",
      "position:fixed;bottom:16px;right:16px;z-index:99999;background:#065f46;color:#fff;" +
      "font:600 13px/1.4 ui-sans-serif,-apple-system,sans-serif;padding:8px 14px;border-radius:8px;" +
      "box-shadow:0 2px 12px rgba(0,0,0,.25);opacity:0;transition:opacity .2s");
    document.body.appendChild(t);
    requestAnimationFrame(function () { t.style.opacity = "1"; });
    setTimeout(function () {
      t.style.opacity = "0";
      setTimeout(function () { t.remove(); }, 300);
    }, 2000);
  }

  async function refresh(v) {
    var res = await fetch("/p/" + slug, { cache: "no-store" });
    if (!res.ok) return;
    var doc = new DOMParser().parseFromString(await res.text(), "text/html");
    // A newer event may have finished while this response was in flight.
    if (v !== cur) return;
    if (doc.head.innerHTML !== document.head.innerHTML) { location.reload(); return; }
    var y = window.scrollY;
    document.body.replaceWith(document.adoptNode(doc.body));
    window.scrollTo(0, y);
    toast("Updated to v" + v);
  }

  var es = new EventSource("/api/plans/" + slug + "/events?since=" + cur);
  es.onmessage = function (e) {
    var v;
    try { v = JSON.parse(e.data).latest; } catch (_) { return; }
    if (!v || v <= cur) return;
    cur = v;
    refresh(v).catch(function () {});
  };
})();
</script>`

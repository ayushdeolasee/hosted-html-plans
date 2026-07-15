package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHubLatestWins(t *testing.T) {
	h := NewHub()
	ch := h.Subscribe("a")
	defer h.Unsubscribe("a", ch)

	// Nobody is draining: the second publish must replace the first rather
	// than be dropped, so a viewer always converges on the newest version.
	h.Publish("a", 4)
	h.Publish("a", 5)

	select {
	case v := <-ch:
		if v != 5 {
			t.Fatalf("got v%d, want the newest (v5)", v)
		}
	default:
		t.Fatal("no version delivered")
	}
}

func TestHubPublishNeverBlocks(t *testing.T) {
	h := NewHub()
	ch := h.Subscribe("a")
	defer h.Unsubscribe("a", ch)

	done := make(chan struct{})
	go func() {
		for i := 1; i <= 100; i++ {
			h.Publish("a", i)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on an undrained subscriber")
	}
}

func TestHubUnsubscribeIsolatesSlugs(t *testing.T) {
	h := NewHub()
	a := h.Subscribe("a")
	b := h.Subscribe("b")
	defer h.Unsubscribe("b", b)

	h.Unsubscribe("a", a)
	if got := h.Count("a"); got != 0 {
		t.Fatalf("slug a has %d subscribers after unsubscribe, want 0", got)
	}
	if got := h.Count("b"); got != 1 {
		t.Fatalf("slug b has %d subscribers, want 1", got)
	}

	// Publishing to a slug with no subscribers must not panic.
	h.Publish("a", 2)
}

// createTestPlan posts samplePlan and returns its slug.
func createTestPlan(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, body := do(t, "POST", ts.URL+"/api/plans", samplePlan)
	if resp.StatusCode != 200 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	slug, _ := decode(t, body)["slug"].(string)
	if slug == "" {
		t.Fatalf("no slug in create response: %s", body)
	}
	return slug
}

// openStream opens the SSE endpoint and returns a channel yielding "data:"
// lines, plus a cancel func that closes the connection.
func openStream(t *testing.T, ts *httptest.Server, slug string, since int) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	url := ts.URL + "/api/plans/" + slug + "/events?since=" + strconv.Itoa(since)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	lines := make(chan string, 4)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return lines, cancel
}

func wantEvent(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case line := <-lines:
		if !strings.Contains(line, want) {
			t.Fatalf("got %q, want it to contain %q", line, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no SSE event containing %q within 3s "+
			"(buffered stream, or Publish never fired?)", want)
	}
}

// TestEventsStreamOnWrite drives the real router end to end: open the stream,
// commit a version over the HTTP API, require the event to arrive on the open
// connection. This is what catches a missing Flush on the logging
// middleware's response wrapper — with the stream buffered, the read below
// blocks until the timeout.
func TestEventsStreamOnWrite(t *testing.T) {
	_, ts := newTestServer(t)
	slug := createTestPlan(t, ts)

	lines, cancel := openStream(t, ts, slug, 1)
	defer cancel()

	// Let the handler subscribe before the write lands.
	time.Sleep(50 * time.Millisecond)

	resp, body := do(t, "POST", ts.URL+"/api/plans/"+slug+"/append", "<p>progress</p>")
	if resp.StatusCode != 200 {
		t.Fatalf("append status=%d body=%s", resp.StatusCode, body)
	}

	wantEvent(t, lines, `"latest":2`)
}

// TestEventsStatusWritePublishes covers the status path specifically: it is
// the call the implement-plan skill makes most often, so it is the one that
// actually drives the live view.
func TestEventsStatusWritePublishes(t *testing.T) {
	_, ts := newTestServer(t)
	slug := createTestPlan(t, ts)

	lines, cancel := openStream(t, ts, slug, 1)
	defer cancel()
	time.Sleep(50 * time.Millisecond)

	resp, body := do(t, "POST", ts.URL+"/api/plans/"+slug+"/status",
		`{"phases":[{"id":1,"status":"done"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status write=%d body=%s", resp.StatusCode, body)
	}

	wantEvent(t, lines, `"latest":2`)
}

// TestEventsCatchUp covers the race where a write lands between page render
// and EventSource connecting: the client says "I have v1", the plan is
// already at v2, and the stream must say so immediately rather than leaving
// the viewer stale until the next write.
func TestEventsCatchUp(t *testing.T) {
	_, ts := newTestServer(t)
	slug := createTestPlan(t, ts)

	if resp, body := do(t, "POST", ts.URL+"/api/plans/"+slug+"/append", "<p>missed</p>"); resp.StatusCode != 200 {
		t.Fatalf("append status=%d body=%s", resp.StatusCode, body)
	}

	lines, cancel := openStream(t, ts, slug, 1)
	defer cancel()

	wantEvent(t, lines, `"latest":2`)
}

// TestEventsNoCatchUpWhenCurrent: a client already on the latest version
// should sit quiet, not receive a spurious event that triggers a pointless
// refetch on every page load.
func TestEventsNoCatchUpWhenCurrent(t *testing.T) {
	_, ts := newTestServer(t)
	slug := createTestPlan(t, ts)

	lines, cancel := openStream(t, ts, slug, 1)
	defer cancel()

	select {
	case line := <-lines:
		t.Fatalf("unexpected event %q for an already-current client", line)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestEventsUnknownPlan404(t *testing.T) {
	_, ts := newTestServer(t)
	resp, _ := do(t, "GET", ts.URL+"/api/plans/nope/events", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestEventsUnsubscribeOnDisconnect guards against a goroutine/channel leak:
// when a viewer closes the tab, the handler must drop its subscription.
func TestEventsUnsubscribeOnDisconnect(t *testing.T) {
	srv, ts := newTestServer(t)
	slug := createTestPlan(t, ts)

	_, cancel := openStream(t, ts, slug, 1)

	waitFor(t, 2*time.Second, func() bool {
		return srv.Store.Events.Count(slug) == 1
	}, "subscriber never registered")

	cancel()

	waitFor(t, 2*time.Second, func() bool {
		return srv.Store.Events.Count(slug) == 0
	}, "subscriber leaked after client disconnect")
}

func TestLiveScriptInjection(t *testing.T) {
	_, ts := newTestServer(t)
	slug := createTestPlan(t, ts)
	if resp, body := do(t, "POST", ts.URL+"/api/plans/"+slug+"/append", "<p>v2</p>"); resp.StatusCode != 200 {
		t.Fatalf("append status=%d body=%s", resp.StatusCode, body)
	}

	// Latest view: gets the live client, primed with the current version.
	_, latest := do(t, "GET", ts.URL+"/p/"+slug, "")
	if !strings.Contains(latest, "data-plans-live") {
		t.Error("latest view is missing the live-reload client")
	}
	if !strings.Contains(latest, "cur = 2") {
		t.Errorf("live client not primed with the current version; body:\n%s", latest)
	}

	// Historical view: frozen by definition. Pinning it is the entire point,
	// so it must not live-update out from under the reader.
	_, old := do(t, "GET", ts.URL+"/p/"+slug+"?version=1", "")
	if strings.Contains(old, "data-plans-live") {
		t.Error("historical view must not get the live-reload client")
	}

	// Text view: consumers parse it as prose; a script would be noise.
	_, text := do(t, "GET", ts.URL+"/p/"+slug+"?format=text", "")
	if strings.Contains(text, "EventSource") {
		t.Error("text view must not get the live-reload client")
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

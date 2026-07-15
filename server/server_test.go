package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// newTestServer builds a Server over a temp data dir with a full router.
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := NewServer(store, DefaultConfig(), NoopFunnel{}, nil)
	ts := httptest.NewServer(srv.FullHandler())
	t.Cleanup(ts.Close)
	return srv, ts
}

const samplePlan = `<!DOCTYPE html><html><head><title>My Feature</title></head>
<body><main><h1>My Feature</h1>
<script type="application/json" id="plan-meta">
{"kind":"implementation-plan","repo":"github.com/x/y","branch":"main","status":"approved",
"phases":[{"id":1,"title":"Core","status":"todo"},{"id":2,"title":"UI","status":"todo"}],
"constraints":["no docker"]}
</script>
</main></body></html>`

func do(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("newrequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("json decode %q: %v", body, err)
	}
	return m
}

func TestCreateAndNewVersionSameSlug(t *testing.T) {
	_, ts := newTestServer(t)

	resp, body := do(t, "POST", ts.URL+"/api/plans", samplePlan)
	if resp.StatusCode != 200 {
		t.Fatalf("create status=%d body=%s", resp.StatusCode, body)
	}
	m := decode(t, body)
	if m["slug"] != "my-feature" {
		t.Fatalf("slug = %v, want my-feature", m["slug"])
	}
	if m["version"].(float64) != 1 {
		t.Fatalf("version = %v, want 1", m["version"])
	}
	urls := m["urls"].(map[string]any)
	if !strings.HasSuffix(urls["lan"].(string), "/p/my-feature") {
		t.Fatalf("lan url = %v", urls["lan"])
	}

	// Second POST with explicit same slug => version 2 on the same plan.
	resp, body = do(t, "POST", ts.URL+"/api/plans?slug=my-feature", samplePlan)
	m = decode(t, body)
	if m["version"].(float64) != 2 {
		t.Fatalf("second version = %v, want 2", m["version"])
	}
	if m["slug"] != "my-feature" {
		t.Fatalf("slug = %v", m["slug"])
	}
}

func TestSlugSameTitleIsNewVersion(t *testing.T) {
	_, ts := newTestServer(t)
	// Same title (=> same generated slug) with no explicit slug => new version.
	do(t, "POST", ts.URL+"/api/plans?title=Same+Name", "<html><title>Same Name</title><body></body></html>")
	_, body := do(t, "POST", ts.URL+"/api/plans?title=Same+Name", "<html><title>Same Name</title><body></body></html>")
	m := decode(t, body)
	if m["slug"] != "same-name" || m["version"].(float64) != 2 {
		t.Fatalf("same-title POST => slug=%v version=%v, want same-name v2", m["slug"], m["version"])
	}
}

func TestSlugDedupeAgainstTrash(t *testing.T) {
	_, ts := newTestServer(t)
	// Create then trash a plan; a new plan with the same title dedupes.
	do(t, "POST", ts.URL+"/api/plans?title=Same+Name", "<html><title>Same Name</title><body></body></html>")
	do(t, "DELETE", ts.URL+"/api/plans/same-name", "")
	_, body := do(t, "POST", ts.URL+"/api/plans?title=Same+Name", "<html><title>Same Name</title><body></body></html>")
	m := decode(t, body)
	if m["slug"] != "same-name-2" {
		t.Fatalf("deduped slug = %v, want same-name-2", m["slug"])
	}
}

func TestPutOptimisticLocking(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)

	// Correct base_version=1 => accepted, becomes v2.
	resp, body := do(t, "PUT", ts.URL+"/api/plans/doc?base_version=1", samplePlan)
	if resp.StatusCode != 200 {
		t.Fatalf("put base=1 status=%d body=%s", resp.StatusCode, body)
	}
	if decode(t, body)["version"].(float64) != 2 {
		t.Fatalf("expected v2, got %s", body)
	}

	// Stale base_version=1 (latest is now 2) => 409 with current.
	resp, body = do(t, "PUT", ts.URL+"/api/plans/doc?base_version=1", samplePlan)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale put status=%d, want 409; body=%s", resp.StatusCode, body)
	}
	if decode(t, body)["current"].(float64) != 2 {
		t.Fatalf("conflict current = %s, want 2", body)
	}

	// Omitted base_version => forced accept, becomes v3, flagged forced.
	resp, body = do(t, "PUT", ts.URL+"/api/plans/doc", samplePlan)
	if resp.StatusCode != 200 {
		t.Fatalf("forced put status=%d body=%s", resp.StatusCode, body)
	}
	if decode(t, body)["version"].(float64) != 3 {
		t.Fatalf("forced put version = %s, want 3", body)
	}
	_, hbody := do(t, "GET", ts.URL+"/api/plans/doc/history", "")
	if !strings.Contains(hbody, `"forced":true`) {
		t.Fatalf("history missing forced flag: %s", hbody)
	}
}

func TestAppendBeforeMainAndSerializes(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)

	resp, _ := do(t, "POST", ts.URL+"/api/plans/doc/append", `<p id="note">appended note</p>`)
	if resp.StatusCode != 200 {
		t.Fatalf("append status=%d", resp.StatusCode)
	}
	_, html := do(t, "GET", ts.URL+"/p/doc", "")
	notePos := strings.Index(html, `id="note"`)
	mainClose := strings.Index(html, "</main>")
	if notePos < 0 || mainClose < 0 || notePos > mainClose {
		t.Fatalf("appended fragment not before </main>: note=%d main=%d", notePos, mainClose)
	}

	// Concurrent appends must serialize into consecutive versions, all kept.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			do(t, "POST", ts.URL+"/api/plans/doc/append", fmt.Sprintf(`<p>frag-%d</p>`, i))
		}(i)
	}
	wg.Wait()

	_, gp := do(t, "GET", ts.URL+"/api/plans/doc", "")
	// v1 create + 1 append + 10 appends = 12.
	if latest := decode(t, gp)["latest"].(float64); latest != 12 {
		t.Fatalf("latest after concurrent appends = %v, want 12", latest)
	}
	_, hist := do(t, "GET", ts.URL+"/api/plans/doc/history", "")
	var entries []VersionEntry
	json.Unmarshal([]byte(hist), &entries)
	seen := map[int]bool{}
	for _, e := range entries {
		if seen[e.Version] {
			t.Fatalf("duplicate version %d in history", e.Version)
		}
		seen[e.Version] = true
	}
	for v := 1; v <= 12; v++ {
		if !seen[v] {
			t.Fatalf("missing version %d in clean sequence", v)
		}
	}
}

func TestStatusMergeByPhaseID(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)

	resp, _ := do(t, "POST", ts.URL+"/api/plans/doc/status", `{"status":"in-progress","phases":[{"id":2,"status":"done"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status update failed: %d", resp.StatusCode)
	}
	_, gp := do(t, "GET", ts.URL+"/api/plans/doc", "")
	m := decode(t, gp)
	if m["status"] != "in-progress" {
		t.Fatalf("promoted status = %v, want in-progress", m["status"])
	}
	meta := m["meta"].(map[string]any)
	phases := meta["phases"].([]any)
	byID := map[float64]map[string]any{}
	for _, p := range phases {
		pm := p.(map[string]any)
		byID[pm["id"].(float64)] = pm
	}
	if byID[2]["status"] != "done" {
		t.Fatalf("phase 2 status = %v, want done", byID[2]["status"])
	}
	if byID[1]["status"] != "todo" {
		t.Fatalf("phase 1 status = %v, want todo (untouched)", byID[1]["status"])
	}
	// Meta block must be rewritten inside the served HTML too.
	_, htmlv := do(t, "GET", ts.URL+"/p/doc", "")
	if !strings.Contains(htmlv, `"status": "done"`) && !strings.Contains(htmlv, `"status":"done"`) {
		t.Fatalf("HTML plan-meta not rewritten with done: %s", htmlv)
	}
}

func TestHistoryRestoreAndBanner(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", `<html><title>V1</title><body><main>one</main></body></html>`)
	do(t, "PUT", ts.URL+"/api/plans/doc?base_version=1", `<html><title>V2</title><body><main>two</main></body></html>`)

	// Restore v1 => new v3 whose content matches v1.
	resp, body := do(t, "POST", ts.URL+"/api/plans/doc/restore?version=1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("restore status=%d body=%s", resp.StatusCode, body)
	}
	if decode(t, body)["version"].(float64) != 3 {
		t.Fatalf("restore version = %s, want 3", body)
	}
	_, latest := do(t, "GET", ts.URL+"/p/doc", "")
	if !strings.Contains(latest, "one") {
		t.Fatalf("restored latest should contain v1 content: %s", latest)
	}

	// Version banner injected for a non-latest view.
	_, v1 := do(t, "GET", ts.URL+"/p/doc?version=1", "")
	if !strings.Contains(v1, "Viewing v1 of 3") || !strings.Contains(v1, `href="/p/doc"`) {
		t.Fatalf("banner missing: %s", v1)
	}
	// Latest view has no banner.
	if strings.Contains(latest, "Viewing v") {
		t.Fatalf("latest view should not carry a banner")
	}

	// History action names.
	_, hist := do(t, "GET", ts.URL+"/api/plans/doc/history", "")
	for _, want := range []string{ActionCreate, ActionRevise, ActionRestore} {
		if !strings.Contains(hist, `"action":"`+want+`"`) {
			t.Fatalf("history missing action %q: %s", want, hist)
		}
	}
}

// versionsOf returns the version numbers in a plan's history, ascending.
func versionsOf(t *testing.T, ts *httptest.Server, slug string) []int {
	t.Helper()
	_, body := do(t, "GET", ts.URL+"/api/plans/"+slug+"/history", "")
	var entries []VersionEntry
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("history decode %q: %v", body, err)
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Version)
	}
	sort.Ints(out)
	return out
}

// pushVersions creates slug at v1 and forces it up to n versions total.
func pushVersions(t *testing.T, ts *httptest.Server, slug string, n int) {
	t.Helper()
	do(t, "POST", ts.URL+"/api/plans?slug="+slug, `<html><title>V1</title><body><main>one</main></body></html>`)
	for i := 2; i <= n; i++ {
		do(t, "PUT", ts.URL+fmt.Sprintf("/api/plans/%s?base_version=%d", slug, i-1),
			fmt.Sprintf(`<html><title>V%d</title><body><main>body-%d</main></body></html>`, i, i))
	}
}

func TestDeleteVersion(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		wantStatus int
		wantLeft   []int
	}{
		{"middle version", "2", 200, []int{1, 3}},
		{"oldest version", "1", 200, []int{2, 3}},
		{"latest is refused", "3", http.StatusConflict, []int{1, 2, 3}},
		{"nonexistent version", "9", http.StatusNotFound, []int{1, 2, 3}},
		{"garbage version", "abc", http.StatusBadRequest, []int{1, 2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, ts := newTestServer(t)
			pushVersions(t, ts, "doc", 3)

			resp, body := do(t, "DELETE", ts.URL+"/api/plans/doc/history/"+tc.version, "")
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.wantStatus, body)
			}
			if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, tc.wantLeft) {
				t.Fatalf("history = %v, want %v", got, tc.wantLeft)
			}
			// The latest version always stays servable.
			if resp, _ := do(t, "GET", ts.URL+"/p/doc", ""); resp.StatusCode != 200 {
				t.Fatalf("latest not served after delete: %d", resp.StatusCode)
			}
		})
	}
}

func TestDeleteVersionRemovesBlobAndPlanIs404(t *testing.T) {
	srv, ts := newTestServer(t)
	pushVersions(t, ts, "doc", 3)

	if resp, _ := do(t, "GET", ts.URL+"/p/doc?version=2", ""); resp.StatusCode != 200 {
		t.Fatalf("v2 not served before delete: %d", resp.StatusCode)
	}
	if resp, body := do(t, "DELETE", ts.URL+"/api/plans/doc/history/2", ""); resp.StatusCode != 200 {
		t.Fatalf("delete v2 status=%d body=%s", resp.StatusCode, body)
	}
	// Blob is gone from disk (hard delete, no second trash tier).
	if _, err := os.Stat(srv.Store.versionPath("doc", 2)); !os.IsNotExist(err) {
		t.Fatalf("v2 blob still on disk: err=%v", err)
	}
	if resp, _ := do(t, "GET", ts.URL+"/p/doc?version=2", ""); resp.StatusCode != 404 {
		t.Fatalf("deleted version still served: %d", resp.StatusCode)
	}
	// A missing blob must not block removing the index entry.
	_ = os.Remove(srv.Store.versionPath("doc", 1))
	if resp, body := do(t, "DELETE", ts.URL+"/api/plans/doc/history/1", ""); resp.StatusCode != 200 {
		t.Fatalf("delete of entry with missing blob: status=%d body=%s", resp.StatusCode, body)
	}
	if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("history = %v, want [3]", got)
	}
	// Unknown plan.
	if resp, _ := do(t, "DELETE", ts.URL+"/api/plans/nope/history/1", ""); resp.StatusCode != 404 {
		t.Fatalf("delete on unknown plan should 404")
	}
}

func TestPruneHistory(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantLeft []int
		pruned   int
	}{
		{"default keeps only the latest", "", []int{5}, 4},
		{"keep=1 keeps only the latest", "?keep=1", []int{5}, 4},
		{"keep=3 keeps the three newest", "?keep=3", []int{3, 4, 5}, 2},
		{"keep exceeds history", "?keep=99", []int{1, 2, 3, 4, 5}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := newTestServer(t)
			pushVersions(t, ts, "doc", 5)

			resp, body := do(t, "DELETE", ts.URL+"/api/plans/doc/history"+tc.query, "")
			if resp.StatusCode != 200 {
				t.Fatalf("prune status=%d body=%s", resp.StatusCode, body)
			}
			if n := decode(t, body)["pruned"].(float64); int(n) != tc.pruned {
				t.Fatalf("pruned = %v, want %d (body=%s)", n, tc.pruned, body)
			}
			if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, tc.wantLeft) {
				t.Fatalf("history = %v, want %v", got, tc.wantLeft)
			}
			// Pruned blobs are gone; kept blobs remain.
			kept := map[int]bool{}
			for _, v := range tc.wantLeft {
				kept[v] = true
			}
			for v := 1; v <= 5; v++ {
				_, err := os.Stat(srv.Store.versionPath("doc", v))
				if kept[v] && err != nil {
					t.Fatalf("kept v%d blob missing: %v", v, err)
				}
				if !kept[v] && !os.IsNotExist(err) {
					t.Fatalf("pruned v%d blob still on disk", v)
				}
			}
			if resp, _ := do(t, "GET", ts.URL+"/p/doc", ""); resp.StatusCode != 200 {
				t.Fatalf("latest not served after prune: %d", resp.StatusCode)
			}
		})
	}
}

func TestPruneHistoryBadKeepAndUnknownPlan(t *testing.T) {
	_, ts := newTestServer(t)
	pushVersions(t, ts, "doc", 2)

	for _, q := range []string{"?keep=0", "?keep=-1", "?keep=x"} {
		if resp, body := do(t, "DELETE", ts.URL+"/api/plans/doc/history"+q, ""); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("prune %s status=%d, want 400; body=%s", q, resp.StatusCode, body)
		}
	}
	if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("history mutated by a rejected prune: %v", got)
	}
	if resp, _ := do(t, "DELETE", ts.URL+"/api/plans/nope/history", ""); resp.StatusCode != 404 {
		t.Fatalf("prune on unknown plan should 404")
	}
}

// The whole point of the guardrail: version numbers are never renumbered or
// reused, so a write after a delete must NOT reoccupy the freed number.
func TestVersionNumbersStayMonotonicAfterDeletes(t *testing.T) {
	_, ts := newTestServer(t)
	pushVersions(t, ts, "doc", 3) // v1, v2, v3

	do(t, "DELETE", ts.URL+"/api/plans/doc/history/2", "")

	// Next write is v4, not v3 (which is taken) and not v2 (which is freed).
	_, body := do(t, "PUT", ts.URL+"/api/plans/doc?base_version=3",
		`<html><title>V4</title><body><main>four</main></body></html>`)
	if v := decode(t, body)["version"].(float64); v != 4 {
		t.Fatalf("version after delete = %v, want 4", v)
	}
	if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, []int{1, 3, 4}) {
		t.Fatalf("history = %v, want [1 3 4] (sparse, no renumbering)", got)
	}

	// Same for derived writes (append/status/restore) — they share the path.
	_, body = do(t, "POST", ts.URL+"/api/plans/doc/append", `<p>frag</p>`)
	if v := decode(t, body)["version"].(float64); v != 5 {
		t.Fatalf("append version = %v, want 5", v)
	}

	// And after a prune down to the latest: next write continues from there.
	do(t, "DELETE", ts.URL+"/api/plans/doc/history", "")
	if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("history after prune = %v, want [5]", got)
	}
	_, body = do(t, "POST", ts.URL+"/api/plans/doc/append", `<p>frag2</p>`)
	if v := decode(t, body)["version"].(float64); v != 6 {
		t.Fatalf("append after prune = %v, want 6", v)
	}
}

// Restore must tolerate a sparse history: v2 is gone, v1 is not.
func TestRestoreAgainstSparseHistory(t *testing.T) {
	_, ts := newTestServer(t)
	pushVersions(t, ts, "doc", 3)

	if resp, _ := do(t, "DELETE", ts.URL+"/api/plans/doc/history/2", ""); resp.StatusCode != 200 {
		t.Fatal("delete v2 failed")
	}

	// Restoring the surviving v1 works and lands as v4.
	resp, body := do(t, "POST", ts.URL+"/api/plans/doc/restore?version=1", "")
	if resp.StatusCode != 200 {
		t.Fatalf("restore v1 status=%d body=%s", resp.StatusCode, body)
	}
	if v := decode(t, body)["version"].(float64); v != 4 {
		t.Fatalf("restore landed at v%v, want 4", v)
	}
	_, latest := do(t, "GET", ts.URL+"/p/doc", "")
	if !strings.Contains(latest, "one") {
		t.Fatalf("restored latest should carry v1 content: %s", latest)
	}
	// Restoring the deleted v2 is a 404, not a panic or a silent empty write.
	if resp, _ := do(t, "POST", ts.URL+"/api/plans/doc/restore?version=2", ""); resp.StatusCode != 404 {
		t.Fatalf("restore of a deleted version = %d, want 404", resp.StatusCode)
	}
	if got := versionsOf(t, ts, "doc"); !reflect.DeepEqual(got, []int{1, 3, 4}) {
		t.Fatalf("history = %v, want [1 3 4]", got)
	}
}

// format=raw is what `push-plan pull` fetches, and whatever it returns gets
// pushed straight back as the next version — so it must carry none of the
// markup the ordinary view injects, on both latest and historical versions.
func TestFormatRaw(t *testing.T) {
	srv, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)
	do(t, "PUT", ts.URL+"/api/plans/doc", strings.Replace(samplePlan, "My Feature", "My Feature v2", 1))

	for _, tc := range []struct {
		name, url string
		version   int
	}{
		{"latest", ts.URL + "/p/doc?format=raw", 2},
		{"historical", ts.URL + "/p/doc?format=raw&version=1", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, "GET", tc.url, "")
			if resp.StatusCode != 200 {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Fatalf("content-type = %q, want text/html", ct)
			}
			if strings.Contains(body, "data-plans-live") {
				t.Fatalf("raw view carries the live-reload client: %s", body)
			}
			if strings.Contains(body, "jump to latest") {
				t.Fatalf("raw view carries the historical banner: %s", body)
			}
			// Byte-identical to the blob on disk: no injection, no rewrite.
			_, want, err := srv.Store.GetVersion("doc", tc.version)
			if err != nil {
				t.Fatalf("GetVersion(%d): %v", tc.version, err)
			}
			if body != string(want) {
				t.Fatalf("raw v%d differs from stored bytes:\n got: %s\nwant: %s", tc.version, body, want)
			}
		})
	}

	// The ordinary view still injects — otherwise this test proves nothing.
	if _, view := do(t, "GET", ts.URL+"/p/doc", ""); !strings.Contains(view, "data-plans-live") {
		t.Fatal("plain /p/{slug} no longer injects the live client; this test is vacuous")
	}
}

func TestFormatText(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)
	resp, text := do(t, "GET", ts.URL+"/p/doc?format=text", "")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("format=text content-type = %q", ct)
	}
	if strings.Contains(text, "<h1>") || strings.Contains(text, "<script") {
		t.Fatalf("text view still has tags: %s", text)
	}
	if !strings.Contains(text, "My Feature") {
		t.Fatalf("text view missing heading text: %s", text)
	}
	// plan-meta JSON kept as text.
	if !strings.Contains(text, "implementation-plan") {
		t.Fatalf("text view dropped plan-meta content: %s", text)
	}
}

func TestListFilters(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=alpha&repo=r1&branch=main", `<html><title>Alpha</title><body></body></html>`)
	do(t, "POST", ts.URL+"/api/plans?slug=beta&repo=r2&branch=dev", `<html><title>Beta</title><body></body></html>`)

	check := func(query string, wantSlugs ...string) {
		_, body := do(t, "GET", ts.URL+"/api/plans"+query, "")
		var plans []Plan
		json.Unmarshal([]byte(body), &plans)
		got := map[string]bool{}
		for _, p := range plans {
			got[p.Slug] = true
		}
		if len(got) != len(wantSlugs) {
			t.Fatalf("query %q returned %v, want %v", query, got, wantSlugs)
		}
		for _, s := range wantSlugs {
			if !got[s] {
				t.Fatalf("query %q missing %q; got %v", query, s, got)
			}
		}
	}
	check("?repo=r1", "alpha")
	check("?branch=dev", "beta")
	check("?q=alph", "alpha")
	check("", "alpha", "beta")
}

func TestTrashArchiveRestorePurge(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)

	// Archive.
	resp, _ := do(t, "DELETE", ts.URL+"/api/plans/doc", "")
	if resp.StatusCode != 200 {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/p/doc", ""); resp.StatusCode != 404 {
		t.Fatalf("archived plan still served: %d", resp.StatusCode)
	}
	_, trash := do(t, "GET", ts.URL+"/api/trash", "")
	if !strings.Contains(trash, `"slug":"doc"`) {
		t.Fatalf("trash list missing doc: %s", trash)
	}

	// Restore from trash.
	resp, _ = do(t, "POST", ts.URL+"/api/trash/doc/restore", "")
	if resp.StatusCode != 200 {
		t.Fatalf("trash restore status=%d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/p/doc", ""); resp.StatusCode != 200 {
		t.Fatalf("restored plan not served: %d", resp.StatusCode)
	}

	// Delete again, then purge.
	do(t, "DELETE", ts.URL+"/api/plans/doc", "")
	resp, _ = do(t, "DELETE", ts.URL+"/api/trash/doc", "")
	if resp.StatusCode != 200 {
		t.Fatalf("purge status=%d", resp.StatusCode)
	}
	_, trash = do(t, "GET", ts.URL+"/api/trash", "")
	if strings.Contains(trash, `"slug":"doc"`) {
		t.Fatalf("purged plan still in trash: %s", trash)
	}
}

func TestSlugRenameRedirect(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=old-name", samplePlan)

	resp, body := do(t, "PATCH", ts.URL+"/api/plans/old-name", `{"slug":"new-name","title":"Renamed"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("rename status=%d body=%s", resp.StatusCode, body)
	}
	if decode(t, body)["slug"] != "new-name" {
		t.Fatalf("renamed slug = %s", body)
	}
	// New slug serves.
	if resp, _ := do(t, "GET", ts.URL+"/p/new-name", ""); resp.StatusCode != 200 {
		t.Fatalf("new slug not served: %d", resp.StatusCode)
	}
	// Old slug 301-redirects to new.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("GET", ts.URL+"/p/old-name", nil)
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("old slug get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("old slug status = %d, want 301", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/p/new-name" {
		t.Fatalf("redirect Location = %q, want /p/new-name", loc)
	}
}

func TestShareCreateRevoke(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)

	resp, body := do(t, "POST", ts.URL+"/api/plans/doc/share", "")
	if resp.StatusCode != 200 {
		t.Fatalf("share status=%d", resp.StatusCode)
	}
	token, _ := decode(t, body)["token"].(string)
	if len(token) < 20 {
		t.Fatalf("token too short: %q", token)
	}
	// Share view serves latest with CSP header.
	resp, sbody := do(t, "GET", ts.URL+"/share/"+token, "")
	if resp.StatusCode != 200 {
		t.Fatalf("share view status=%d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("share view missing CSP header")
	}
	if !strings.Contains(sbody, "My Feature") {
		t.Fatalf("share view wrong content")
	}

	// Revoke => token 404s.
	resp, _ = do(t, "DELETE", ts.URL+"/api/plans/doc/share", "")
	if resp.StatusCode != 200 {
		t.Fatalf("unshare status=%d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/share/"+token, ""); resp.StatusCode != 404 {
		t.Fatalf("revoked share still served: %d", resp.StatusCode)
	}
}

func TestSharesOnlyRouter(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, DefaultConfig(), NoopFunnel{}, nil)
	full := httptest.NewServer(srv.FullHandler())
	defer full.Close()
	shares := httptest.NewServer(srv.SharesHandler())
	defer shares.Close()

	do(t, "POST", full.URL+"/api/plans?slug=doc", samplePlan)
	_, sb := do(t, "POST", full.URL+"/api/plans/doc/share", "")
	token := decode(t, sb)["token"].(string)

	// Shares router serves the one route.
	if resp, _ := do(t, "GET", shares.URL+"/share/"+token, ""); resp.StatusCode != 200 {
		t.Fatalf("shares router did not serve /share: %d", resp.StatusCode)
	}
	// Shares router must NOT expose the API, homepage, or /p/.
	for _, path := range []string{"/", "/api/plans", "/p/doc", "/healthz"} {
		if resp, _ := do(t, "GET", shares.URL+path, ""); resp.StatusCode != 404 {
			t.Fatalf("shares router leaked %q (status %d)", path, resp.StatusCode)
		}
	}
	// Writes must not exist on the shares router.
	if resp, _ := do(t, "POST", shares.URL+"/api/plans", samplePlan); resp.StatusCode != 404 {
		t.Fatalf("shares router leaked write route (status %d)", resp.StatusCode)
	}
}

func TestSizeCap(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, MaxPlanBytes+1)
	if _, _, err := store.Create(big, WriteParams{Slug: "big"}); err != ErrTooLarge {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	resp, body := do(t, "GET", ts.URL+"/healthz", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("healthz = %d %s", resp.StatusCode, body)
	}
}

func TestHomepageServesEmbeddedUI(t *testing.T) {
	_, ts := newTestServer(t)

	resp, body := do(t, "GET", ts.URL+"/", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET / status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / content-type = %q, want text/html", ct)
	}
	if !strings.Contains(body, `<script src="/app.js">`) {
		t.Fatalf("GET / did not serve the embedded index.html: %s", body)
	}

	resp, body = do(t, "GET", ts.URL+"/app.js", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /app.js status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("GET /app.js content-type = %q", ct)
	}
	if !strings.Contains(body, "function api(") {
		t.Fatalf("GET /app.js did not serve the embedded app.js: %s", body)
	}
}

func TestTitleAutoExtract(t *testing.T) {
	_, ts := newTestServer(t)
	do(t, "POST", ts.URL+"/api/plans?slug=doc", samplePlan)
	_, gp := do(t, "GET", ts.URL+"/api/plans/doc", "")
	if decode(t, gp)["title"] != "My Feature" {
		t.Fatalf("auto title = %v, want My Feature", decode(t, gp)["title"])
	}
}

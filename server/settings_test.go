package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

// ---- fakes for the tailnet manager seam ----

// markerFunnel is a distinguishable FunnelController so tests can assert the
// manager wired the tailnet's funnel into the server (vs the NoopFunnel it
// reverts to on stop).
type markerFunnel struct{ NoopFunnel }

// fakeTailnetHandle stands in for *Tailnet so manager start/stop can be tested
// without a real tailnet.
type fakeTailnetHandle struct {
	mu     sync.Mutex
	st     TailnetState
	closed bool
}

func (f *fakeTailnetHandle) Funnel() FunnelController { return markerFunnel{} }
func (f *fakeTailnetHandle) Status() TailnetState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *fakeTailnetHandle) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// managedSetup wires a Server + TailnetManager whose "make a Tailnet" step is
// stubbed to hand back a fake in a known state. dataDir is where the manager
// persists the tailnet state snapshot.
type managedSetup struct {
	srv     *Server
	mgr     *TailnetManager
	dataDir string
	mu      sync.Mutex
	last    *fakeTailnetHandle
	authURL string
}

func newManagedServer(t *testing.T, cfg Config) *managedSetup {
	t.Helper()
	t.Setenv("PLANS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := NewServer(store, cfg, NoopFunnel{}, discardLogger())
	ms := &managedSetup{srv: srv, dataDir: t.TempDir(), authURL: "https://login.tailscale.com/a/testauth"}
	mgr := NewTailnetManager(context.Background(), TailnetManagerDeps{
		DataDir:      ms.dataDir,
		Logger:       discardLogger(),
		Server:       srv,
		ActiveShares: store.ActiveShareCount,
	})
	mgr.startFn = func(ctx context.Context, d TailnetDeps) (tailnetHandle, error) {
		f := &fakeTailnetHandle{st: TailnetState{
			Enabled: true, State: TailnetStateWaitingAuth, AuthURL: ms.authURL,
		}}
		ms.mu.Lock()
		ms.last = f
		ms.mu.Unlock()
		return f, nil
	}
	srv.SetTailnetManager(mgr)
	ms.mgr = mgr
	return ms
}

func (ms *managedSetup) lastHandle() *fakeTailnetHandle {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.last
}

// ---- settings round-trip + validation ----

func TestSettingsGetPutRoundTrip(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	// GET reflects defaults; tailnet disabled; no restart required.
	resp, body := do(t, "GET", ts.URL+"/api/settings", "")
	if resp.StatusCode != 200 {
		t.Fatalf("GET settings status=%d body=%s", resp.StatusCode, body)
	}
	m := decode(t, body)
	if m["hostname"] != "plans" || m["tailscale_enabled"] != false {
		t.Fatalf("unexpected settings: %s", body)
	}
	if m["restart_required"] != false {
		t.Fatalf("restart_required should start false: %s", body)
	}
	tn := m["tailnet"].(map[string]any)
	if tn["state"] != TailnetStateDisabled {
		t.Fatalf("tailnet state = %v, want disabled", tn["state"])
	}
	// Must not leak other config keys.
	if _, leaked := m["tailnet_domain"]; leaked {
		t.Fatalf("settings leaked tailnet_domain: %s", body)
	}

	// PUT a partial update (hostname only) — round-trips on the next GET.
	resp, body = do(t, "PUT", ts.URL+"/api/settings", `{"hostname":"my-box"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("PUT status=%d body=%s", resp.StatusCode, body)
	}
	m = decode(t, body)
	if m["hostname"] != "my-box" {
		t.Fatalf("PUT response hostname = %v", m["hostname"])
	}
	_, body = do(t, "GET", ts.URL+"/api/settings", "")
	if decode(t, body)["hostname"] != "my-box" {
		t.Fatalf("hostname did not persist: %s", body)
	}
	// It was persisted to disk too.
	if got := ms.srv.config().Hostname; got != "my-box" {
		t.Fatalf("in-memory config hostname = %q", got)
	}
	persisted, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if persisted.Hostname != "my-box" {
		t.Fatalf("disk config hostname = %q", persisted.Hostname)
	}
}

func TestSettingsValidationErrors(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	cases := []struct{ name, body string }{
		{"bad-hostname-leading-hyphen", `{"hostname":"-nope"}`},
		{"bad-hostname-dot", `{"hostname":"a.b"}`},
		{"bad-hostname-empty", `{"hostname":""}`},
		{"bad-lan-no-port", `{"lan_listen":"127.0.0.1"}`},
		{"bad-lan-garbage", `{"lan_listen":"not a listen addr"}`},
		{"bad-lan-port-range", `{"lan_listen":"127.0.0.1:99999"}`},
		{"malformed-json", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, body := do(t, "PUT", ts.URL+"/api/settings", c.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, body)
			}
			if decode(t, body)["error"] == nil {
				t.Fatalf("expected an error message, got %s", body)
			}
		})
	}

	// A rejected PUT must not have mutated config.
	if ms.srv.config().Hostname != "plans" {
		t.Fatalf("config mutated by an invalid PUT: %q", ms.srv.config().Hostname)
	}

	// Valid "off" and host:port forms are accepted.
	for _, ok := range []string{`{"lan_listen":"off"}`, `{"lan_listen":":8080"}`, `{"lan_listen":"0.0.0.0:8080"}`} {
		resp, body := do(t, "PUT", ts.URL+"/api/settings", ok)
		if resp.StatusCode != 200 {
			t.Fatalf("valid lan_listen %s rejected: %d %s", ok, resp.StatusCode, body)
		}
	}
}

// ---- restart_required lifecycle for lan_listen ----

func TestSettingsLANListenRestartRequired(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	// Changing lan_listen flags restart_required in the response...
	_, body := do(t, "PUT", ts.URL+"/api/settings", `{"lan_listen":"127.0.0.1:9090"}`)
	if decode(t, body)["restart_required"] != true {
		t.Fatalf("restart_required should be true after lan_listen change: %s", body)
	}
	// ...and stays sticky on subsequent GETs.
	_, body = do(t, "GET", ts.URL+"/api/settings", "")
	if decode(t, body)["restart_required"] != true {
		t.Fatalf("restart_required should remain true: %s", body)
	}

	// Setting lan_listen to the SAME value again doesn't clear it (still sticky).
	_, body = do(t, "PUT", ts.URL+"/api/settings", `{"lan_listen":"127.0.0.1:9090"}`)
	if decode(t, body)["restart_required"] != true {
		t.Fatalf("restart_required cleared unexpectedly: %s", body)
	}
}

// ---- live tailscale_enabled toggle through the manager seam ----

func TestSettingsTailscaleLiveToggle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	// Enable: starts the tailnet live, no restart required, funnel wired in.
	resp, body := do(t, "PUT", ts.URL+"/api/settings", `{"tailscale_enabled":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("enable status=%d body=%s", resp.StatusCode, body)
	}
	m := decode(t, body)
	if m["tailscale_enabled"] != true || m["restart_required"] != false {
		t.Fatalf("enable response: %s", body)
	}
	if !ms.mgr.Running() {
		t.Fatal("manager should be running after enable")
	}
	if _, ok := ms.srv.Funnel.(markerFunnel); !ok {
		t.Fatalf("server funnel not wired to tailnet, got %T", ms.srv.Funnel)
	}

	// The waiting-for-auth URL surfaces via both endpoints.
	tn := m["tailnet"].(map[string]any)
	if tn["state"] != TailnetStateWaitingAuth || tn["auth_url"] != ms.authURL {
		t.Fatalf("settings tailnet block: %v", tn)
	}
	_, body = do(t, "GET", ts.URL+"/api/tailnet/status", "")
	st := decode(t, body)
	if st["state"] != TailnetStateWaitingAuth || st["auth_url"] != ms.authURL {
		t.Fatalf("tailnet status endpoint: %s", body)
	}

	// A live state transition (auth completes) is reflected promptly by the
	// status endpoint, read from the live manager not the disk snapshot.
	h := ms.lastHandle()
	h.mu.Lock()
	h.st = TailnetState{Enabled: true, State: TailnetStateAuthenticated, NodeName: "my-box.tailnet.ts.net", Tailnet: "tailnet.ts.net"}
	h.mu.Unlock()
	_, body = do(t, "GET", ts.URL+"/api/tailnet/status", "")
	st = decode(t, body)
	if st["state"] != TailnetStateAuthenticated || st["fqdn"] != "my-box.tailnet.ts.net" || st["domain"] != "tailnet.ts.net" {
		t.Fatalf("authenticated status: %s", body)
	}

	// Disable: stops the tailnet live, reverts funnel to noop, persists the
	// disabled snapshot to disk (so `plans service status` stays accurate).
	resp, body = do(t, "PUT", ts.URL+"/api/settings", `{"tailscale_enabled":false}`)
	if resp.StatusCode != 200 {
		t.Fatalf("disable status=%d body=%s", resp.StatusCode, body)
	}
	if ms.mgr.Running() {
		t.Fatal("manager should be stopped after disable")
	}
	if !h.closed {
		t.Fatal("tailnet handle should have been Closed on disable")
	}
	if _, ok := ms.srv.Funnel.(NoopFunnel); !ok {
		t.Fatalf("funnel should revert to NoopFunnel, got %T", ms.srv.Funnel)
	}
	if decode(t, body)["tailnet"].(map[string]any)["state"] != TailnetStateDisabled {
		t.Fatalf("disable response tailnet state: %s", body)
	}
	disk, err := ReadTailnetState(ms.dataDir)
	if err != nil {
		t.Fatalf("ReadTailnetState: %v", err)
	}
	if disk.State != TailnetStateDisabled {
		t.Fatalf("disk snapshot state = %q, want disabled", disk.State)
	}
}

// ---- hostname change semantics ----

func TestSettingsHostnameChangeRestartWhenRunning(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = true
	ms := newManagedServer(t, cfg)
	if err := ms.mgr.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	// Hostname change while the tailnet is running => restart_required.
	_, body := do(t, "PUT", ts.URL+"/api/settings", `{"hostname":"renamed"}`)
	m := decode(t, body)
	if m["restart_required"] != true {
		t.Fatalf("hostname change while running should require restart: %s", body)
	}
	if !ms.mgr.Running() {
		t.Fatal("hostname change must not tear down the running tailnet")
	}
}

func TestSettingsHostnameChangeNoRestartWhenStopped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	ts := httptest.NewServer(ms.srv.FullHandler())
	t.Cleanup(ts.Close)

	_, body := do(t, "PUT", ts.URL+"/api/settings", `{"hostname":"renamed"}`)
	if decode(t, body)["restart_required"] != false {
		t.Fatalf("hostname change while stopped should NOT require restart: %s", body)
	}
}

// ---- settings must be private-only ----

func TestSettingsAbsentFromSharesRouter(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TailscaleEnabled = false
	ms := newManagedServer(t, cfg)
	shares := httptest.NewServer(ms.srv.SharesHandler())
	t.Cleanup(shares.Close)

	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/settings"},
		{"PUT", "/api/settings"},
		{"GET", "/api/tailnet/status"},
	} {
		resp, _ := do(t, ep.method, shares.URL+ep.path, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on shares router = %d, want 404 (settings must be private-only)", ep.method, ep.path, resp.StatusCode)
		}
	}
}

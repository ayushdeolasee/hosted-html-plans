package server

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeFunnelProvider stands in for tsnet's ListenFunnel so the funnel
// open/close/reconcile lifecycle can be exercised without a real tailnet. It
// hands out real loopback listeners (so Serve genuinely works) and records
// how many it created, or fails with a preset error.
type fakeFunnelProvider struct {
	mu       sync.Mutex
	opens    int
	failWith error
	lastAddr string
}

func (p *fakeFunnelProvider) ListenFunnel() (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failWith != nil {
		return nil, p.failWith
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p.opens++
	p.lastAddr = ln.Addr().String()
	return ln, nil
}

func (p *fakeFunnelProvider) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens
}

func (p *fakeFunnelProvider) addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastAddr
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
}

func TestFunnelEnsureOpenIdempotent(t *testing.T) {
	p := &fakeFunnelProvider{}
	shares := int64(1)
	f := NewFunnel(p, okHandler(), func() int { return int(atomic.LoadInt64(&shares)) }, nil, discardLogger())

	if err := f.EnsureOpen(); err != nil {
		t.Fatalf("first EnsureOpen: %v", err)
	}
	if !f.IsOpen() {
		t.Fatal("funnel should be open after EnsureOpen")
	}
	// Second call must not open a second listener.
	if err := f.EnsureOpen(); err != nil {
		t.Fatalf("second EnsureOpen: %v", err)
	}
	if got := p.openCount(); got != 1 {
		t.Fatalf("expected exactly 1 listener opened, got %d", got)
	}

	// The listener actually serves the shares handler.
	resp, err := http.Get("http://" + p.addr() + "/share/anything")
	if err != nil {
		t.Fatalf("GET funnel listener: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("funnel handler status = %d, want 200", resp.StatusCode)
	}
	_ = f.Close()
}

func TestFunnelCloseIfIdle(t *testing.T) {
	p := &fakeFunnelProvider{}
	shares := int64(2)
	f := NewFunnel(p, okHandler(), func() int { return int(atomic.LoadInt64(&shares)) }, nil, discardLogger())

	if err := f.EnsureOpen(); err != nil {
		t.Fatalf("EnsureOpen: %v", err)
	}

	// Shares still active => CloseIfIdle is a no-op.
	if err := f.CloseIfIdle(); err != nil {
		t.Fatalf("CloseIfIdle (active): %v", err)
	}
	if !f.IsOpen() {
		t.Fatal("funnel must stay open while shares remain")
	}

	// Drop to zero => CloseIfIdle tears it down.
	atomic.StoreInt64(&shares, 0)
	if err := f.CloseIfIdle(); err != nil {
		t.Fatalf("CloseIfIdle (idle): %v", err)
	}
	if f.IsOpen() {
		t.Fatal("funnel must close when no shares remain")
	}

	// The old listener is really gone.
	if _, err := http.Get("http://" + p.addr() + "/share/x"); err == nil {
		t.Fatal("expected connection failure after funnel close")
	}

	// Reopen works after a close.
	atomic.StoreInt64(&shares, 1)
	if err := f.EnsureOpen(); err != nil {
		t.Fatalf("reopen EnsureOpen: %v", err)
	}
	if !f.IsOpen() {
		t.Fatal("funnel should reopen")
	}
	if got := p.openCount(); got != 2 {
		t.Fatalf("expected 2 total opens across the lifecycle, got %d", got)
	}
	_ = f.Close()
}

func TestFunnelReconcileClosedWhenIdle(t *testing.T) {
	p := &fakeFunnelProvider{}
	// Reconcile-on-startup logic: only open when ActiveShares > 0. Model the
	// "no shares" branch — the caller wouldn't even call EnsureOpen, but if it
	// did after a drop-to-zero, CloseIfIdle keeps things at zero exposure.
	f := NewFunnel(p, okHandler(), func() int { return 0 }, nil, discardLogger())

	if err := f.CloseIfIdle(); err != nil {
		t.Fatalf("CloseIfIdle on never-opened funnel: %v", err)
	}
	if f.IsOpen() {
		t.Fatal("funnel should not be open")
	}
	if got := p.openCount(); got != 0 {
		t.Fatalf("no listener should have been opened, got %d", got)
	}
}

func TestFunnelNotReadySurfacesError(t *testing.T) {
	p := &fakeFunnelProvider{}
	notReady := errors.New("public URL will go live once the tailnet is connected")
	f := NewFunnel(p, okHandler(), func() int { return 1 }, func() error { return notReady }, discardLogger())

	err := f.EnsureOpen()
	if err == nil {
		t.Fatal("EnsureOpen should return the not-ready error")
	}
	if !errors.Is(err, notReady) {
		t.Fatalf("expected the readiness error, got %v", err)
	}
	if f.IsOpen() {
		t.Fatal("funnel must not open when not ready")
	}
	if got := p.openCount(); got != 0 {
		t.Fatalf("no listener should open when not ready, got %d", got)
	}
}

func TestFunnelACLPermissionErrorIsActionable(t *testing.T) {
	p := &fakeFunnelProvider{failWith: errors.New("Funnel not available; requires the funnel node attribute")}
	f := NewFunnel(p, okHandler(), func() int { return 1 }, nil, discardLogger())

	err := f.EnsureOpen()
	if err == nil {
		t.Fatal("EnsureOpen should fail when the provider refuses the funnel")
	}
	msg := err.Error()
	if !strings.Contains(msg, "funnel node attribute") && !strings.Contains(msg, "\"funnel\" node attribute") {
		t.Fatalf("error should name the funnel ACL attribute, got: %v", err)
	}
	if !strings.Contains(msg, "acls") {
		t.Fatalf("error should point at the ACL policy, got: %v", err)
	}
}

func TestFunnelConcurrentOpenClose(t *testing.T) {
	p := &fakeFunnelProvider{}
	var shares int64 = 1
	f := NewFunnel(p, okHandler(), func() int { return int(atomic.LoadInt64(&shares)) }, nil, discardLogger())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = f.EnsureOpen() }()
		go func() {
			defer wg.Done()
			// Flip shares to exercise the idle path under contention.
			if atomic.LoadInt64(&shares) == 1 {
				atomic.StoreInt64(&shares, 0)
				_ = f.CloseIfIdle()
				atomic.StoreInt64(&shares, 1)
			}
		}()
	}
	wg.Wait()
	// End in a deterministic state.
	atomic.StoreInt64(&shares, 0)
	if err := f.CloseIfIdle(); err != nil {
		t.Fatalf("final CloseIfIdle: %v", err)
	}
	if f.IsOpen() {
		t.Fatal("funnel should be closed at the end")
	}
}

// ---- small helpers ----

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

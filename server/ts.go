package server

// ts.go implements the embedded Tailscale integration (plan.html §2–4):
//
//   - an in-process tsnet node named "plans" that joins the tailnet, gets
//     automatic Let's Encrypt TLS, and serves the full app on :443;
//   - an on-demand public funnel listener on :8443 bound to the shares-only
//     router, opened when a plan is shared and closed when the last share is
//     revoked (Funnel, implementing FunnelController).
//
// Everything here runs only when Config.TailscaleEnabled is true. When it is
// false, main.go never calls StartTailnet and none of this code executes, so
// the process stays LAN-only with the ~10 MB footprint of plan.html §7.
//
// The funnel lifecycle logic (open / close / reconcile) is deliberately kept
// behind the funnelProvider interface so it can be unit-tested with a fake
// listener provider, without a real tailnet (see ts_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Tailnet connection/auth states persisted for `plans service status`.
const (
	TailnetStateDisabled      = "disabled"
	TailnetStateStarting      = "starting"
	TailnetStateWaitingAuth   = "waiting-for-auth"
	TailnetStateAuthenticated = "authenticated"
)

// TailnetState is the persisted snapshot of the tsnet node's auth/connection
// state, written to <data-dir>/tsnet-status.json so `plans service status`
// can report real state without touching tsnet itself.
type TailnetState struct {
	Enabled   bool      `json:"enabled"`
	State     string    `json:"state"`
	AuthURL   string    `json:"auth_url,omitempty"`
	NodeName  string    `json:"node_name,omitempty"` // FQDN, e.g. plans.tailnet.ts.net
	Tailnet   string    `json:"tailnet,omitempty"`   // MagicDNS suffix, e.g. tailnet.ts.net
	Err       string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TailnetStatePath is where the tailnet status snapshot lives.
func TailnetStatePath(dataDir string) string {
	return filepath.Join(dataDir, "tsnet-status.json")
}

// WriteTailnetState atomically persists the tailnet status snapshot.
func WriteTailnetState(dataDir string, st TailnetState) error {
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	return atomicWrite(TailnetStatePath(dataDir), append(data, '\n'), 0o644)
}

// ReadTailnetState reads the persisted tailnet status snapshot.
func ReadTailnetState(dataDir string) (TailnetState, error) {
	data, err := os.ReadFile(TailnetStatePath(dataDir))
	if err != nil {
		return TailnetState{}, err
	}
	var st TailnetState
	if err := json.Unmarshal(data, &st); err != nil {
		return TailnetState{}, err
	}
	return st, nil
}

// ---- on-demand funnel controller ----

// funnelProvider opens the public funnel listener. It abstracts the single
// tsnet call the funnel lifecycle depends on so the open/close/reconcile
// logic is testable with a fake provider (no real tailnet required).
type funnelProvider interface {
	ListenFunnel() (net.Listener, error)
}

// Funnel is the on-demand public funnel controller (plan.html §4). It
// implements server.FunnelController. The listener exists only while at
// least one plan is shared: EnsureOpen creates it, CloseIfIdle tears it down
// once no active shares remain, giving the box zero public exposure at rest.
type Funnel struct {
	provider     funnelProvider
	handler      http.Handler
	activeShares func() int
	ready        func() error // non-nil error => tailnet not ready yet
	logger       *log.Logger

	mu  sync.Mutex
	ln  net.Listener
	srv *http.Server
}

// NewFunnel builds a Funnel. ready may be nil (treated as always-ready).
func NewFunnel(provider funnelProvider, handler http.Handler, activeShares func() int, ready func() error, logger *log.Logger) *Funnel {
	if logger == nil {
		logger = log.Default()
	}
	if ready == nil {
		ready = func() error { return nil }
	}
	if activeShares == nil {
		activeShares = func() int { return 0 }
	}
	return &Funnel{provider: provider, handler: handler, activeShares: activeShares, ready: ready, logger: logger}
}

// EnsureOpen opens the funnel listener if it isn't already open. Idempotent
// and safe for concurrent use. If the tailnet isn't connected/authenticated
// yet, it returns a descriptive (non-fatal to sharing) error; if tsnet
// refuses the funnel (missing "funnel" ACL node attribute), it returns an
// actionable error naming the fix.
func (f *Funnel) EnsureOpen() error {
	if err := f.ready(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		return nil // already open
	}
	ln, err := f.provider.ListenFunnel()
	if err != nil {
		return funnelListenError(err)
	}
	srv := &http.Server{Handler: f.handler, ReadHeaderTimeout: 10 * time.Second}
	f.ln = ln
	f.srv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.logger.Printf("funnel serve error: %v", err)
		}
	}()
	f.logger.Printf("public funnel listener opened on :8443 (shared plans now reachable from the internet)")
	return nil
}

// CloseIfIdle closes the funnel listener if no active shares remain. It is a
// no-op if the listener isn't open or shares still exist. Thread-safe against
// concurrent EnsureOpen.
func (f *Funnel) CloseIfIdle() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln == nil {
		return nil
	}
	if f.activeShares() > 0 {
		return nil
	}
	f.logger.Printf("last share revoked; closing public funnel listener (zero public exposure)")
	return f.closeLocked()
}

// Close unconditionally closes the funnel listener (used at shutdown).
func (f *Funnel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeLocked()
}

// IsOpen reports whether the funnel listener is currently open.
func (f *Funnel) IsOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ln != nil
}

// closeLocked fully closes the listener and its server. Caller holds f.mu.
func (f *Funnel) closeLocked() error {
	if f.srv == nil {
		return nil
	}
	// srv.Close closes the listener and all active connections immediately —
	// the listener is torn down fully, not merely stopped accepting.
	err := f.srv.Close()
	f.srv = nil
	f.ln = nil
	return err
}

// funnelListenError wraps a ListenFunnel error, adding an actionable hint
// when the failure looks like a missing funnel permission (plan.html §3, §4:
// the "funnel" node attribute must be granted in the tailnet ACL policy).
func funnelListenError(err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "funnel") {
		return fmt.Errorf("cannot open public funnel (%w): grant the \"funnel\" node attribute to this node in your tailnet ACL policy at https://login.tailscale.com/admin/acls, then retry the share", err)
	}
	return fmt.Errorf("cannot open public funnel: %w", err)
}

// tsFunnelProvider is the real, tsnet-backed funnelProvider.
type tsFunnelProvider struct{ ts *tsnet.Server }

func (p tsFunnelProvider) ListenFunnel() (net.Listener, error) {
	// Funnel only supports 443/8443/10000; :443 is taken by the tailnet
	// listener, so shares go on :8443 (plan.html §3).
	return p.ts.ListenFunnel("tcp", ":8443")
}

// ---- tsnet node orchestration ----

// TailnetDeps are the server-layer dependencies the tailnet bring-up needs.
type TailnetDeps struct {
	Hostname      string       // tailnet node name (default "plans")
	DataDir       string       // app data dir; tsnet state lives under <DataDir>/tsnet
	Logger        *log.Logger  // request/status logger
	FullHandler   http.Handler // served on the tailnet :443 listener
	SharesHandler http.Handler // served on the funnel :8443 listener
	ActiveShares  func() int   // live count of shared plans (reconcile + close checks)
	// OnConnected is invoked once, when the node is authenticated and its
	// identity is known: node is the label ("plans"), domain the MagicDNS
	// suffix ("tailnet.ts.net"). Used to switch URL generation to absolute
	// https URLs.
	OnConnected func(node, domain string)
}

// Tailnet owns the embedded tsnet node, the tailnet :443 listener, and the
// on-demand funnel. Construct it with StartTailnet.
type Tailnet struct {
	ts     *tsnet.Server
	deps   TailnetDeps
	funnel *Funnel

	mu     sync.Mutex
	state  TailnetState
	tlsSrv *http.Server
	closed bool
}

// StartTailnet creates the tsnet node and begins bringing it up in the
// background (auth is a one-time human step and must not block startup). It
// returns immediately; the returned Tailnet's Funnel() is usable right away
// (EnsureOpen simply reports "not connected yet" until auth completes).
func StartTailnet(ctx context.Context, deps TailnetDeps) (*Tailnet, error) {
	if deps.Logger == nil {
		deps.Logger = log.Default()
	}
	if deps.Hostname == "" {
		deps.Hostname = "plans"
	}
	tsDir := filepath.Join(deps.DataDir, "tsnet")
	if err := os.MkdirAll(tsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create tsnet state dir: %w", err)
	}
	ts := &tsnet.Server{
		Hostname: deps.Hostname,
		Dir:      tsDir,
		// Backend logs are verbose (MagicSock, LocalBackend); discard them to
		// keep to the request-line-only default of plan.html §7. We capture
		// the auth URL and state ourselves via the IPN bus.
		Logf:     func(string, ...any) {},
		UserLogf: func(format string, args ...any) { deps.Logger.Printf("tsnet: "+format, args...) },
	}
	tn := &Tailnet{ts: ts, deps: deps}
	tn.funnel = NewFunnel(tsFunnelProvider{ts}, deps.SharesHandler, deps.ActiveShares, tn.readyErr, deps.Logger)
	tn.setState(TailnetState{State: TailnetStateStarting})
	go tn.bringUp(ctx)
	return tn, nil
}

// Funnel returns the on-demand funnel controller to wire into the Server.
// It returns the FunnelController interface so *Tailnet satisfies the
// tailnetHandle seam used by TailnetManager.
func (tn *Tailnet) Funnel() FunnelController { return tn.funnel }

// Status returns a snapshot of the current in-memory tailnet state (auth URL
// while waiting, FQDN/domain once authenticated). This is the live source of
// truth the settings/status API reads, independent of the disk snapshot.
func (tn *Tailnet) Status() TailnetState {
	tn.mu.Lock()
	defer tn.mu.Unlock()
	return tn.state
}

// setState records and persists the current tailnet state.
func (tn *Tailnet) setState(st TailnetState) {
	st.Enabled = true
	tn.mu.Lock()
	tn.state = st
	tn.mu.Unlock()
	if err := WriteTailnetState(tn.deps.DataDir, st); err != nil {
		tn.deps.Logger.Printf("persist tailnet state: %v", err)
	}
}

// readyErr is the funnel's readiness gate: until the node is authenticated,
// sharing succeeds but the public listener can't open, so we return a clear
// message the share API surfaces as a warning (plan.html §4, task item 5).
func (tn *Tailnet) readyErr() error {
	tn.mu.Lock()
	state := tn.state.State
	tn.mu.Unlock()
	if state == TailnetStateAuthenticated {
		return nil
	}
	return fmt.Errorf("public URL will go live once the tailnet is connected (tailnet state: %s)", state)
}

// bringUp starts tsnet and watches the IPN bus for the auth URL and the
// transition to Running, then wires up the tailnet listeners.
func (tn *Tailnet) bringUp(ctx context.Context) {
	if err := tn.ts.Start(); err != nil {
		tn.deps.Logger.Printf("tsnet: start failed: %v", err)
		tn.setState(TailnetState{State: TailnetStateStarting, Err: err.Error()})
		return
	}
	lc, err := tn.ts.LocalClient()
	if err != nil {
		tn.deps.Logger.Printf("tsnet: local client: %v", err)
		return
	}
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		if ctx.Err() == nil {
			tn.deps.Logger.Printf("tsnet: watch ipn bus: %v", err)
		}
		return
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			return // context cancelled or server closing
		}
		if n.BrowseToURL != nil {
			tn.announceAuthURL(*n.BrowseToURL)
		}
		if n.State != nil {
			switch *n.State {
			case ipn.NeedsLogin:
				if err := lc.StartLoginInteractive(ctx); err != nil && ctx.Err() == nil {
					tn.deps.Logger.Printf("tsnet: start login: %v", err)
				}
			case ipn.Running:
				tn.onConnected(ctx)
				return
			}
		}
	}
}

// announceAuthURL logs the first-run auth URL prominently and persists it so
// `plans service status` can surface it (plan.html §5 first-run flow).
func (tn *Tailnet) announceAuthURL(url string) {
	tn.mu.Lock()
	already := tn.state.AuthURL == url
	tn.mu.Unlock()
	if already {
		return
	}
	l := tn.deps.Logger
	l.Printf("")
	l.Printf("=====================================================================")
	l.Printf("  Tailscale: authenticate the 'plans' node — open this URL once:")
	l.Printf("      %s", url)
	l.Printf("  (also reported by `plans service status`)")
	l.Printf("=====================================================================")
	l.Printf("")
	tn.setState(TailnetState{State: TailnetStateWaitingAuth, AuthURL: url})
}

// onConnected runs once the node reaches Running: it records identity, starts
// the tailnet :443 listener with automatic TLS, and reconciles the funnel.
func (tn *Tailnet) onConnected(ctx context.Context) {
	st, err := tn.ts.Up(ctx)
	if err != nil {
		if ctx.Err() == nil {
			tn.deps.Logger.Printf("tsnet: up: %v", err)
		}
		return
	}
	fqdn := nodeFQDN(st)
	node, domain := splitFQDN(fqdn, tn.deps.Hostname)
	tn.setState(TailnetState{State: TailnetStateAuthenticated, NodeName: fqdn, Tailnet: domain})
	if tn.deps.OnConnected != nil && domain != "" {
		tn.deps.OnConnected(node, domain)
	}
	tn.deps.Logger.Printf("tsnet: connected as %s (tailnet %s)", fqdn, domain)

	// Tailnet :443 listener — full app, automatic Let's Encrypt TLS.
	ln, err := tn.ts.ListenTLS("tcp", ":443")
	if err != nil {
		tn.deps.Logger.Printf("tsnet: listen :443: %v", err)
	} else {
		srv := &http.Server{Handler: tn.deps.FullHandler, ReadHeaderTimeout: 10 * time.Second}
		tn.mu.Lock()
		if tn.closed {
			tn.mu.Unlock()
			_ = ln.Close()
			return
		}
		tn.tlsSrv = srv
		tn.mu.Unlock()
		go func() {
			if fqdn != "" {
				tn.deps.Logger.Printf("serving full app on https://%s (tailnet)", fqdn)
			}
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				tn.deps.Logger.Printf("tsnet: :443 serve error: %v", err)
			}
		}()
	}

	// Reconcile the funnel on startup: open iff active shares already exist
	// (plan.html §4 restart resilience).
	if n := tn.deps.ActiveShares(); n > 0 {
		if err := tn.funnel.EnsureOpen(); err != nil {
			tn.deps.Logger.Printf("reconcile funnel: %v", err)
		} else {
			tn.deps.Logger.Printf("reconciled public funnel: reopened for %d active share(s)", n)
		}
	}
}

// Close shuts the tailnet down cleanly: the funnel listener, the :443
// listener, and the tsnet node itself.
func (tn *Tailnet) Close() error {
	tn.mu.Lock()
	tn.closed = true
	srv := tn.tlsSrv
	tn.tlsSrv = nil
	tn.mu.Unlock()

	var firstErr error
	if tn.funnel != nil {
		if err := tn.funnel.Close(); err != nil {
			firstErr = err
		}
	}
	if srv != nil {
		if err := srv.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := tn.ts.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// nodeFQDN returns the node's fully-qualified MagicDNS name without the
// trailing dot (e.g. "plans.tailnet.ts.net"), or "" if unknown.
func nodeFQDN(st *ipnstate.Status) string {
	if st != nil && st.Self != nil && st.Self.DNSName != "" {
		return strings.TrimSuffix(st.Self.DNSName, ".")
	}
	return ""
}

// splitFQDN splits "plans.tailnet.ts.net" into node ("plans") and domain
// suffix ("tailnet.ts.net"). Falls back to fallbackNode when the FQDN is
// empty. Deriving the node label from the FQDN (rather than assuming the
// configured hostname) keeps URLs correct even if the control plane deduped
// the name (e.g. "plans-1").
func splitFQDN(fqdn, fallbackNode string) (node, domain string) {
	if fqdn == "" {
		return fallbackNode, ""
	}
	if i := strings.Index(fqdn, "."); i >= 0 {
		return fqdn[:i], fqdn[i+1:]
	}
	return fqdn, ""
}

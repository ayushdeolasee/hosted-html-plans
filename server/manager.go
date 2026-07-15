package server

// manager.go owns the runtime lifecycle of the embedded Tailscale node. Where
// ts.go implements a single Tailnet instance, TailnetManager makes starting
// and stopping one a runtime operation the web UI (settings API) can drive:
//
//   - boot: main.go calls Start once when Config.TailscaleEnabled is true;
//   - live enable/disable: PUT /api/settings toggles tailscale_enabled, which
//     calls Start / Disable on the same manager — one code path;
//   - shutdown: main.go calls Stop for a graceful close without clobbering the
//     persisted "authenticated" snapshot `plans service status` reads.
//
// The one tsnet-touching step (building + starting a Tailnet) is behind the
// startFn seam so tests can stub it and exercise the enable/disable lifecycle
// without a real tailnet (see manager_test.go).

import (
	"context"
	"log"
	"net/http"
	"sync"
)

// tailnetHandle is the subset of *Tailnet the manager depends on. Abstracting
// it lets tests substitute a fake for the "make a Tailnet" step.
type tailnetHandle interface {
	Funnel() FunnelController
	Status() TailnetState
	Close() error
}

// TailnetManagerDeps are the fixed dependencies a manager needs to bring a
// tailnet up. Hostname is NOT here: it is read live from the server config at
// each Start so hostname changes take effect on the next (re)start.
type TailnetManagerDeps struct {
	DataDir       string
	Logger        *log.Logger
	Server        *Server      // funnel + identity wiring live here
	FullHandler   http.Handler // served on the tailnet :443 listener
	SharesHandler http.Handler // served on the funnel :8443 listener
	ActiveShares  func() int
}

// TailnetManager owns the current Tailnet instance and serializes runtime
// start/stop so UI toggles can't race a shutdown.
type TailnetManager struct {
	deps    TailnetManagerDeps
	baseCtx context.Context
	// startFn builds and starts a tailnet. Defaults to a StartTailnet wrapper;
	// overridden in tests.
	startFn func(context.Context, TailnetDeps) (tailnetHandle, error)

	mu     sync.Mutex
	tn     tailnetHandle
	cancel context.CancelFunc
}

// NewTailnetManager builds a manager. baseCtx bounds every tailnet started by
// the manager (cancelled at process shutdown); each Start also gets its own
// child context cancelled by the matching Stop.
func NewTailnetManager(baseCtx context.Context, deps TailnetManagerDeps) *TailnetManager {
	if deps.Logger == nil {
		deps.Logger = log.Default()
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	m := &TailnetManager{deps: deps, baseCtx: baseCtx}
	m.startFn = func(ctx context.Context, d TailnetDeps) (tailnetHandle, error) {
		return StartTailnet(ctx, d)
	}
	return m
}

// Start brings the tailnet up if it isn't already running. Idempotent: a
// second call while running is a no-op. It reads the hostname from the live
// server config, wires the funnel controller and tailnet identity into the
// server exactly as boot did, and begins auth in the background (returns
// immediately; watch the log / status endpoint for the auth URL).
func (m *TailnetManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tn != nil {
		return nil // already running
	}
	hostname := m.deps.Server.config().Hostname
	ctx, cancel := context.WithCancel(m.baseCtx)
	deps := TailnetDeps{
		Hostname:      hostname,
		DataDir:       m.deps.DataDir,
		Logger:        m.deps.Logger,
		FullHandler:   m.deps.FullHandler,
		SharesHandler: m.deps.SharesHandler,
		ActiveShares:  m.deps.ActiveShares,
		OnConnected: func(node, domain string) {
			m.deps.Server.SetTailnetIdentity(node, domain)
		},
	}
	tn, err := m.startFn(ctx, deps)
	if err != nil {
		cancel()
		return err
	}
	m.tn = tn
	m.cancel = cancel
	m.deps.Server.Funnel = tn.Funnel()
	return nil
}

// Stop gracefully closes the running tailnet for process shutdown. It reverts
// the funnel controller to a no-op and clears the tailnet identity, but does
// NOT overwrite the persisted state snapshot — so `plans service status` still
// reports the last authenticated state while the service is merely stopped.
func (m *TailnetManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked()
}

// Disable stops the tailnet AND records the "disabled" state on disk. This is
// the live-disable path (user turned Tailscale off in settings): the snapshot
// `plans service status` reads must reflect that it is intentionally off.
func (m *TailnetManager) Disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.stopLocked()
	if werr := WriteTailnetState(m.deps.DataDir, TailnetState{State: TailnetStateDisabled}); werr != nil {
		m.deps.Logger.Printf("persist disabled tailnet state: %v", werr)
		if err == nil {
			err = werr
		}
	}
	return err
}

// stopLocked tears down the current tailnet. Caller holds m.mu.
func (m *TailnetManager) stopLocked() error {
	if m.tn == nil {
		return nil
	}
	tn := m.tn
	cancel := m.cancel
	m.tn = nil
	m.cancel = nil
	if cancel != nil {
		cancel()
	}
	err := tn.Close()
	// Revert URL generation to relative and stop opening funnels.
	m.deps.Server.Funnel = NoopFunnel{}
	m.deps.Server.ClearTailnetIdentity()
	return err
}

// Running reports whether a tailnet is currently up.
func (m *TailnetManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tn != nil
}

// Status returns the live tailnet state. When stopped it reports the disabled
// state directly (not a stale disk snapshot); when running it reflects the
// in-memory state including the auth URL while waiting and the FQDN/domain once
// authenticated.
func (m *TailnetManager) Status() TailnetState {
	m.mu.Lock()
	tn := m.tn
	m.mu.Unlock()
	if tn == nil {
		return TailnetState{Enabled: false, State: TailnetStateDisabled}
	}
	return tn.Status()
}

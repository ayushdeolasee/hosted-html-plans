package server

// settings.go implements the settings + tailnet-status API consumed by the
// web UI (a separate agent builds the page). All three routes are registered
// on the FULL/private router only (FullHandler); they must never appear on the
// shares/funnel router (SharesHandler), which stays a single read-only route.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
)

// settingsView is the GET /api/settings response. It exposes only the three
// user-editable config fields plus derived tailnet/restart status — never any
// other on-disk config (e.g. tailnet_domain is deliberately omitted).
type settingsView struct {
	LANListen        string      `json:"lan_listen"`
	TailscaleEnabled bool        `json:"tailscale_enabled"`
	Hostname         string      `json:"hostname"`
	Tailnet          tailnetView `json:"tailnet"`
	RestartRequired  bool        `json:"restart_required"`
}

// tailnetView is the GET /api/tailnet/status response and the nested "tailnet"
// object in settings. Fields appear only when meaningful for the state.
type tailnetView struct {
	State   string `json:"state"`              // disabled|starting|waiting-for-auth|authenticated
	AuthURL string `json:"auth_url,omitempty"` // set while waiting-for-auth
	FQDN    string `json:"fqdn,omitempty"`     // set once authenticated
	Domain  string `json:"domain,omitempty"`   // MagicDNS suffix, set once authenticated
}

// settingsUpdate is the PUT /api/settings body. Pointer fields distinguish
// "field omitted" (leave as-is) from "field set to zero value".
type settingsUpdate struct {
	LANListen        *string `json:"lan_listen"`
	TailscaleEnabled *bool   `json:"tailscale_enabled"`
	Hostname         *string `json:"hostname"`
}

// tailnetStatusView builds the tailnet portion from the live manager state.
func (s *Server) tailnetStatusView() tailnetView {
	st := TailnetState{State: TailnetStateDisabled}
	if s.tailnet != nil {
		st = s.tailnet.Status()
	}
	v := tailnetView{State: st.State}
	if v.State == "" {
		v.State = TailnetStateDisabled
	}
	switch st.State {
	case TailnetStateWaitingAuth:
		v.AuthURL = st.AuthURL
	case TailnetStateAuthenticated:
		v.FQDN = st.NodeName
		v.Domain = st.Tailnet
	}
	return v
}

func (s *Server) settingsView() settingsView {
	cfg := s.config()
	return settingsView{
		LANListen:        cfg.LANListen,
		TailscaleEnabled: cfg.TailscaleEnabled,
		Hostname:         cfg.Hostname,
		Tailnet:          s.tailnetStatusView(),
		RestartRequired:  s.restartRequired.Load(),
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsView())
}

func (s *Server) handleTailnetStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.tailnetStatusView())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var upd settingsUpdate
	if err := json.Unmarshal(body, &upd); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be a JSON object")
		return
	}

	old := s.config()
	next := old

	if upd.LANListen != nil {
		if err := validateLANListen(*upd.LANListen); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		next.LANListen = *upd.LANListen
	}
	if upd.Hostname != nil {
		if err := validateHostname(*upd.Hostname); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		next.Hostname = *upd.Hostname
	}
	if upd.TailscaleEnabled != nil {
		next.TailscaleEnabled = *upd.TailscaleEnabled
	}

	// Persist first so a manager start/stop can't leave disk and memory out of
	// sync; the write is atomic (config.go).
	if err := SaveConfig(next); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist config: "+err.Error())
		return
	}
	s.setConfig(next)

	// Reconcile side effects.
	//
	// lan_listen: rebinding the LAN listener live isn't worth the complexity;
	// persist and flag restart_required (sticky until the process restarts).
	if next.LANListen != old.LANListen {
		s.restartRequired.Store(true)
	}

	// tailscale_enabled: applies LIVE via the manager, no restart needed.
	if s.tailnet != nil && next.TailscaleEnabled != old.TailscaleEnabled {
		if next.TailscaleEnabled {
			if err := s.tailnet.Start(); err != nil {
				writeErr(w, http.StatusInternalServerError, "start tailscale: "+err.Error())
				return
			}
		} else {
			if err := s.tailnet.Disable(); err != nil {
				writeErr(w, http.StatusInternalServerError, "stop tailscale: "+err.Error())
				return
			}
		}
	}

	// hostname: chosen semantics — if the tailnet is (and stays) running, a
	// hostname change requires a restart to re-register the node under the new
	// name; we flag restart_required rather than tearing down and rebuilding a
	// live, authenticated node (the more robust, deterministic choice). If the
	// tailnet is not running, the new hostname is simply picked up at the next
	// Start (including a false->true enable in this same request), so no
	// restart is needed.
	if next.Hostname != old.Hostname && s.tailnet != nil && s.tailnet.Running() && next.TailscaleEnabled == old.TailscaleEnabled {
		s.restartRequired.Store(true)
	}

	writeJSON(w, http.StatusOK, s.settingsView())
}

// ---- validation ----

// dnsLabel matches a single valid DNS label (RFC 1123): 1–63 chars, letters/
// digits/hyphens, not starting or ending with a hyphen.
var dnsLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validateHostname(h string) error {
	if !dnsLabel.MatchString(h) {
		return fmt.Errorf("invalid hostname %q: must be a DNS label (1–63 chars, letters/digits/hyphens, no leading or trailing hyphen)", h)
	}
	return nil
}

func validateLANListen(v string) error {
	if v == "off" {
		return nil
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return fmt.Errorf("invalid lan_listen %q: must be \"off\" or host:port (e.g. 0.0.0.0:8080)", v)
	}
	if port == "" {
		return fmt.Errorf("invalid lan_listen %q: port is required (e.g. 127.0.0.1:8080)", v)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid lan_listen %q: port must be 0–65535", v)
	}
	_ = host // host may be empty (":8080") — that's valid (all interfaces).
	return nil
}

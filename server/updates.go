package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/ayushdeolasee/hosted-html-plans/internal/buildinfo"
	"github.com/ayushdeolasee/hosted-html-plans/internal/selfupdate"
)

const (
	updateIntentHeader = "X-Plans-Update"
	maxUpdateBody      = 4 << 10
)

// UpdateManager is the narrow dependency used by the update handlers. The
// production implementation lives in internal/selfupdate; tests can provide a
// fake without performing a release request, install, or restart.
type UpdateManager interface {
	Check(context.Context) selfupdate.Report
	Install(context.Context) (selfupdate.Report, error)
}

type unavailableUpdates struct{}

func (unavailableUpdates) Check(context.Context) selfupdate.Report {
	return selfupdate.Report{
		CurrentVersion: buildinfo.Version,
		Status:         "unsupported",
		Reason:         "self-update is not configured for this server process",
	}
}

func (u unavailableUpdates) Install(ctx context.Context) (selfupdate.Report, error) {
	return u.Check(ctx), selfupdate.ErrNoUpdate
}

// SetUpdateManager wires the production manager, or a fake in focused handler
// tests. A nil value leaves the conservative unsupported default in place.
func (s *Server) SetUpdateManager(manager UpdateManager) {
	if manager != nil {
		s.updates = manager
	}
}

func (s *Server) updateManager() UpdateManager {
	if s.updates == nil {
		return unavailableUpdates{}
	}
	return s.updates
}

func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.updateManager().Check(r.Context()))
}

func (s *Server) handleInstallUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameOriginUpdateRequest(r) {
		writeErr(w, http.StatusForbidden, "cross-origin update requests are forbidden")
		return
	}
	if err := validateUpdateIntent(w, r); err != nil {
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
		return
	}

	rep, err := s.updateManager().Install(r.Context())
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, selfupdate.ErrInstallBusy) || errors.Is(err, selfupdate.ErrNoUpdate) {
			code = http.StatusConflict
		}
		writeJSON(w, code, rep)
		return
	}
	writeJSON(w, http.StatusAccepted, rep)
}

func sameOriginUpdateRequest(r *http.Request) bool {
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}

func validateUpdateIntent(w http.ResponseWriter, r *http.Request) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	isJSON := err == nil && strings.EqualFold(mediaType, "application/json")
	hasHeader := r.Header.Get(updateIntentHeader) == "install"
	if !isJSON && !hasHeader {
		return errors.New("use application/json or X-Plans-Update: install")
	}

	limited := http.MaxBytesReader(w, r.Body, maxUpdateBody)
	defer limited.Close()
	body, err := io.ReadAll(limited)
	if err != nil {
		return errors.New("update request body exceeds 4096 bytes")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		if hasHeader {
			return nil
		}
		return errors.New("application/json requests must contain an empty object")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil || len(payload) != 0 {
		return errors.New("update request body must be an empty JSON object")
	}
	return nil
}

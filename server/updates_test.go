package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayushdeolasee/hosted-html-plans/internal/selfupdate"
)

type fakeUpdateManager struct {
	checks   int
	installs int
	report   selfupdate.Report
	err      error
}

func (f *fakeUpdateManager) Check(context.Context) selfupdate.Report {
	f.checks++
	return f.report
}

func (f *fakeUpdateManager) Install(context.Context) (selfupdate.Report, error) {
	f.installs++
	return f.report, f.err
}

func updateTestServer(t *testing.T, updates UpdateManager) *httptest.Server {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, DefaultConfig(), NoopFunnel{}, discardLogger())
	srv.SetUpdateManager(updates)
	ts := httptest.NewServer(srv.FullHandler())
	t.Cleanup(ts.Close)
	return ts
}

func TestGetUpdatesContract(t *testing.T) {
	fake := &fakeUpdateManager{report: selfupdate.Report{
		CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0", UpdateAvailable: true,
		Supported: true, Status: "available", Reason: "",
	}}
	ts := updateTestServer(t, fake)
	resp, body := do(t, http.MethodGet, ts.URL+"/api/updates", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	m := decode(t, body)
	for _, key := range []string{"current_version", "latest_version", "update_available", "supported", "status", "reason"} {
		if _, ok := m[key]; !ok {
			t.Errorf("response missing %q: %s", key, body)
		}
	}
}

func TestPostUpdatesRequiresIntentAndSameOrigin(t *testing.T) {
	fake := &fakeUpdateManager{report: selfupdate.Report{Status: "installing"}}
	ts := updateTestServer(t, fake)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/updates", strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("missing intent status=%d, want 415", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/updates", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d, want 403", resp.StatusCode)
	}
	if fake.installs != 0 {
		t.Fatalf("rejected requests invoked Install %d times", fake.installs)
	}
}

func TestPostUpdatesAcceptsJSONOrExplicitHeader(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, header, body string
	}{
		{"json", "application/json; charset=utf-8", "", "{}"},
		{"explicit header", "", "install", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeUpdateManager{report: selfupdate.Report{Status: "installing"}}
			ts := updateTestServer(t, fake)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/updates", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.header != "" {
				req.Header.Set(updateIntentHeader, tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted || fake.installs != 1 {
				t.Fatalf("status=%d installs=%d", resp.StatusCode, fake.installs)
			}
		})
	}
}

func TestPostUpdatesReportsConflicts(t *testing.T) {
	fake := &fakeUpdateManager{report: selfupdate.Report{Status: "unsupported", Reason: "busy"}, err: selfupdate.ErrInstallBusy}
	ts := updateTestServer(t, fake)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/updates", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !errors.Is(fake.err, selfupdate.ErrInstallBusy) || resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want 409", resp.StatusCode)
	}
}

package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ayushdeolasee/hosted-html-plans/internal/buildinfo"
)

func TestHealthReportsRunningVersionWithoutCaching(t *testing.T) {
	w := httptest.NewRecorder()
	(&Server{}).handleHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health response: status=%d headers=%v", w.Code, w.Header())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" || body["version"] != buildinfo.Version {
		t.Fatalf("health must identify the running binary: %v", body)
	}
}

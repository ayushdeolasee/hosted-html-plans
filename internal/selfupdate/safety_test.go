package selfupdate

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type notReleasedSource struct{ fakeSource }

func (*notReleasedSource) latest(context.Context, string) (release, error) {
	return release{}, ErrNoRelease
}

func TestNoPublishedReleaseIsNotAnInstallError(t *testing.T) {
	m, _, _ := testManager("dev", "", false)
	m.source = &notReleasedSource{}
	rep := m.Check(context.Background())
	if rep.Status != "no_release" || rep.UpdateAvailable || rep.CurrentVersion != "dev" {
		t.Fatalf("unexpected empty repository report: %+v", rep)
	}
}

type updateRoundTripper func(*http.Request) (*http.Response, error)

func (f updateRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubNoReleaseResponse(t *testing.T) {
	g := &githubSource{client: &http.Client{Transport: updateRoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)), Header: make(http.Header)}, nil
	})}}
	_, err := g.latest(context.Background(), "plans-darwin-arm64")
	if err != ErrNoRelease {
		t.Fatalf("missing release should be an explicit empty state: %v", err)
	}
}

type successfulSupervisorRunner struct {
	calls int
	name  string
	args  []string
}

func (r *successfulSupervisorRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls++
	r.name = name
	r.args = append([]string(nil), args...)
	return []byte("state = running\npid = 888888\n"), nil
}

func TestSchedulingLaunchesCurrentTargetAsHelper(t *testing.T) {
	for _, tc := range []struct {
		name, kind, command string
	}{
		{name: "launchd", kind: "launchd", command: "launchctl"},
		{name: "systemd", kind: "systemd-user", command: "systemd-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			runner := &successfulSupervisorRunner{}
			s := &osSupervisor{runner: runner, found: serviceSpec{kind: tc.kind, target: "service-target"}}
			req := applyRequest{
				target: filepath.Join(dir, "plans"), staged: filepath.Join(dir, ".plans-update-test"),
				backup: filepath.Join(dir, ".plans-backup-test"), result: filepath.Join(dir, ".plans-update-result-test"), oldPID: os.Getpid(),
			}
			if err := s.schedule(context.Background(), req); err != nil {
				t.Fatalf("schedule: %v", err)
			}
			if runner.calls != 1 || runner.name != tc.command {
				t.Fatalf("runner = %s calls=%d", runner.name, runner.calls)
			}
			separator := -1
			for n, arg := range runner.args {
				if arg == "--" {
					separator = n
					break
				}
			}
			if separator < 0 || separator+1 >= len(runner.args) || runner.args[separator+1] != req.target {
				t.Fatalf("helper args = %q; want target %q after --", runner.args, req.target)
			}
		})
	}
}

func TestActivatedUpdateRetainsOriginalForRecovery(t *testing.T) {
	dir := t.TempDir()
	req := applyRequest{target: filepath.Join(dir, "plans"), staged: filepath.Join(dir, ".plans-update-test"), backup: filepath.Join(dir, ".plans-backup-test"), result: filepath.Join(dir, ".plans-update-result-test"), oldPID: 999999, service: serviceSpec{kind: "launchd", target: "test-service"}}
	for path, body := range map[string]string{req.target: "original", req.staged: "updated", req.backup: ""} {
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyAndRestart(context.Background(), req, &successfulSupervisorRunner{}, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{req.target: "updated", req.backup: "original"} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != expected {
			t.Fatalf("%s: got %q, %v; want %q", path, body, err, expected)
		}
	}
}

func TestHelperResultAcknowledgesFailureAndBoundsUnknownOutcome(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, ".plans-update-result-test")
	wait := waitForApplyResult(result, time.Second)
	acknowledgeApplyResult(result, errors.New("known failure"), log.New(io.Discard, "", 0))
	if err := <-wait; err == nil || err.Error() != "known failure" {
		t.Fatalf("acknowledged error = %v", err)
	}

	err := <-waitForApplyResult(filepath.Join(dir, ".plans-update-result-timeout"), 10*time.Millisecond)
	if !errors.Is(err, errHelperOutcomeUnknown) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestRollbackReportsDirectorySyncFailureAfterRestartAttempt(t *testing.T) {
	dir := t.TempDir()
	req := applyRequest{
		target: filepath.Join(dir, "plans"), staged: filepath.Join(dir, ".plans-update-test"),
		backup: filepath.Join(dir, ".plans-backup-test"), result: filepath.Join(dir, ".plans-update-result-test"),
		oldPID: 999999, service: serviceSpec{kind: "launchd", target: "test-service"},
	}
	if err := os.WriteFile(req.target, []byte("updated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.backup, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	previousSync := syncDirectoryForRollback
	syncDirectoryForRollback = func(string) error { return errors.New("fake sync failure") }
	t.Cleanup(func() { syncDirectoryForRollback = previousSync })
	runner := &successfulSupervisorRunner{}
	err := rollback(req, runner, errors.New("update failed"))
	if err == nil || !strings.Contains(err.Error(), "restored and restarted but directory sync failed") {
		t.Fatalf("rollback error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("restart attempts = %d, want 1", runner.calls)
	}
}

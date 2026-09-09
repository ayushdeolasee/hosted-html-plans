package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu      sync.Mutex
	release release
	files   map[string][]byte
	opens   int
	block   <-chan struct{}
}

func (f *fakeSource) latest(context.Context, string) (release, error) {
	return f.release, nil
}

func (f *fakeSource) open(ctx context.Context, url string, _ int64) (download, error) {
	if f.block != nil && url == f.release.checksumsURL {
		select {
		case <-f.block:
		case <-ctx.Done():
			return download{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	b, ok := f.files[url]
	if !ok {
		return download{}, errors.New("missing fake download")
	}
	return download{body: io.NopCloser(bytes.NewReader(b)), contentLength: int64(len(b))}, nil
}

type fakeInstall struct {
	supported  bool
	reason     string
	staged     bool
	handedOff  bool
	completion chan error
}

func (f *fakeInstall) support(context.Context) (bool, string) {
	return f.supported, f.reason
}

func (f *fakeInstall) stage(_ context.Context, _ release, _ string, body io.Reader) (string, error) {
	_, err := io.Copy(io.Discard, body)
	f.staged = err == nil
	return "/fake/.plans-update-staged", err
}

func (f *fakeInstall) handoff(context.Context, string) (<-chan error, error) {
	f.handedOff = true
	return f.completion, nil
}

func testManager(current, latest string, supported bool) (*Manager, *fakeSource, *fakeInstall) {
	binary := []byte("verified release binary")
	sum := sha256.Sum256(binary)
	asset := "plans-darwin-arm64"
	rel := release{
		version: latest, assetName: asset, assetURL: "asset",
		assetSize: int64(len(binary)), checksumsURL: "checksums",
	}
	source := &fakeSource{release: rel, files: map[string][]byte{
		"asset":     binary,
		"checksums": []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"),
	}}
	install := &fakeInstall{supported: supported, reason: "not an installed service", completion: make(chan error, 1)}
	return &Manager{
		current: current, asset: asset, source: source, install: install,
		logger: log.New(io.Discard, "", 0), cacheTTL: time.Hour,
	}, source, install
}

func TestVersionComparisonIsNumericAndStableOnly(t *testing.T) {
	v110, err := parseStableVersion("v1.10.0+build.4")
	if err != nil {
		t.Fatal(err)
	}
	v129, err := parseStableVersion("1.2.9")
	if err != nil {
		t.Fatal(err)
	}
	if compareVersions(v110, v129) <= 0 {
		t.Fatal("1.10.0 must sort after 1.2.9")
	}
	for _, invalid := range []string{"dev", "v1.2", "v1.2.3-rc.1", "v01.2.3", "v1.two.3"} {
		if _, err := parseStableVersion(invalid); err == nil {
			t.Errorf("parseStableVersion(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestCheckReportsAvailabilityAndUnsupportedBuilds(t *testing.T) {
	tests := []struct {
		name, current, latest, status string
		supported, available          bool
	}{
		{"available", "v1.2.3", "v1.3.0", "available", true, true},
		{"up to date", "v1.3.0", "v1.3.0", "up_to_date", true, false},
		{"development build", "dev", "v1.3.0", "unsupported", false, false},
		{"downgrade", "v2.0.0", "v1.3.0", "unsupported", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := testManager(tc.current, tc.latest, tc.supported)
			rep := m.Check(context.Background())
			if rep.Status != tc.status || rep.Supported != tc.supported || rep.UpdateAvailable != tc.available {
				t.Fatalf("report = %+v", rep)
			}
		})
	}
}

func TestInstallUsesFakeDownloadsAndSerializes(t *testing.T) {
	m, source, install := testManager("v1.0.0", "v1.1.0", true)
	gate := make(chan struct{})
	source.block = gate

	firstDone := make(chan error, 1)
	go func() {
		_, err := m.Install(context.Background())
		firstDone <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		m.installMu.Lock()
		installing := m.installing
		m.installMu.Unlock()
		if installing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first install did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := m.Install(context.Background()); !errors.Is(err, ErrInstallBusy) {
		t.Fatalf("second Install error = %v, want ErrInstallBusy", err)
	}
	close(gate)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if !install.staged || !install.handedOff {
		t.Fatalf("install calls: staged=%v handedOff=%v", install.staged, install.handedOff)
	}
	if source.opens != 2 {
		t.Fatalf("fake downloads = %d, want checksums + asset", source.opens)
	}
}

func TestHelperTerminalFailureAllowsRetry(t *testing.T) {
	m, _, install := testManager("v1.0.0", "v1.1.0", true)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	install.completion <- errors.New("staged executable disappeared")
	waitForManagerState(t, m, false, false)

	rep := m.Check(context.Background())
	if rep.Status != "available" || !rep.Supported || !rep.UpdateAvailable || !strings.Contains(rep.Reason, "you can retry") {
		t.Fatalf("terminal failure report = %+v", rep)
	}
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatalf("retry after known terminal failure: %v", err)
	}
}

func TestUnknownHelperOutcomeBlocksRetry(t *testing.T) {
	m, _, install := testManager("v1.0.0", "v1.1.0", true)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	install.completion <- errHelperOutcomeUnknown
	waitForManagerState(t, m, true, true)

	rep, err := m.Install(context.Background())
	if !errors.Is(err, ErrInstallBusy) || rep.Status != "error" || !strings.Contains(rep.Reason, "do not retry") {
		t.Fatalf("unknown outcome retry = (%+v, %v)", rep, err)
	}
}

func waitForManagerState(t *testing.T, m *Manager, installing, unknown bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		m.installMu.Lock()
		matches := m.installing == installing && m.unknown == unknown
		m.installMu.Unlock()
		if matches {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager state did not become installing=%v unknown=%v", installing, unknown)
		}
		time.Sleep(time.Millisecond)
	}
}

type inertSupervisor struct{}

func (inertSupervisor) detect(context.Context, string, int) (bool, string) { return true, "" }
func (inertSupervisor) schedule(context.Context, applyRequest) error       { return nil }

func TestStageVerifiesSHAAndPreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "plans")
	if err := os.WriteFile(target, []byte("old"), 0o751); err != nil {
		t.Fatal(err)
	}
	i := &executableInstall{target: target, supervisor: inertSupervisor{}, logger: log.New(io.Discard, "", 0)}
	body := []byte("new verified binary")
	sum := sha256.Sum256(body)
	rel := release{assetSize: int64(len(body))}
	staged, err := i.stage(context.Background(), rel, hex.EncodeToString(sum[:]), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(staged) })
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("staged permissions = %o, want 751", info.Mode().Perm())
	}
	if _, err := i.stage(context.Background(), rel, strings.Repeat("0", 64), bytes.NewReader(body)); err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatalf("hash mismatch error = %v", err)
	}
}

type rollbackRunner struct {
	restarts int
}

func (r *rollbackRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "launchctl" && len(args) > 0 && args[0] == "kickstart" {
		r.restarts++
		if r.restarts == 1 {
			return []byte("restart rejected"), errors.New("fake restart failure")
		}
		return nil, nil
	}
	return nil, errors.New("unexpected fake command")
}

func TestApplyRollsBackOriginalWhenRestartFails(t *testing.T) {
	dir := t.TempDir()
	req := applyRequest{
		target:  filepath.Join(dir, "plans"),
		staged:  filepath.Join(dir, ".plans-update-test"),
		backup:  filepath.Join(dir, ".plans-backup-test"),
		result:  filepath.Join(dir, ".plans-update-result-test"),
		oldPID:  999999,
		service: serviceSpec{kind: "launchd", target: "gui/501/com.ayushdeolasee.plans"},
	}
	if err := os.WriteFile(req.target, []byte("original"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.staged, []byte("updated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.backup, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &rollbackRunner{}
	err := applyAndRestart(context.Background(), req, runner, log.New(io.Discard, "", 0))
	if err == nil || !strings.Contains(err.Error(), "original executable restored and restarted") {
		t.Fatalf("applyAndRestart error = %v", err)
	}
	got, err := os.ReadFile(req.target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("target after rollback = %q", got)
	}
	if runner.restarts != 2 {
		t.Fatalf("restart attempts = %d, want 2", runner.restarts)
	}
}

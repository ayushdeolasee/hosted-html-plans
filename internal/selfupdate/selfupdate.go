// Package selfupdate implements explicit, verified updates from the official
// hosted-html-plans GitHub releases.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	maxReleaseMetadata = 1 << 20
	maxChecksums       = 1 << 20
	maxBinary          = 128 << 20
	defaultCacheTTL    = 5 * time.Minute
)

var (
	ErrInstallBusy          = errors.New("an update install is already in progress")
	ErrNoUpdate             = errors.New("no supported update is available")
	ErrNoRelease            = errors.New("no stable release has been published yet")
	errHelperOutcomeUnknown = errors.New("update helper outcome is unknown")
)

// Report is the stable JSON shape returned by both update endpoints.
type Report struct {
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	UpdateAvailable bool   `json:"update_available"`
	Supported       bool   `json:"supported"`
	Status          string `json:"status"`
	Reason          string `json:"reason"`
}

type release struct {
	version      string
	assetName    string
	assetURL     string
	assetSize    int64
	checksumsURL string
}

type download struct {
	body          io.ReadCloser
	contentLength int64
}

type releaseSource interface {
	latest(context.Context, string) (release, error)
	open(context.Context, string, int64) (download, error)
}

type localInstall interface {
	support(context.Context) (bool, string)
	stage(context.Context, release, string, io.Reader) (string, error)
	handoff(context.Context, string) (<-chan error, error)
}

// Manager serializes update attempts and caches GitHub's latest-release
// metadata briefly to avoid turning polling into API-rate-limit pressure.
type Manager struct {
	current  string
	asset    string
	source   releaseSource
	install  localInstall
	logger   *log.Logger
	cacheTTL time.Duration

	cacheMu    sync.Mutex
	cached     release
	cachedAt   time.Time
	installMu  sync.Mutex
	installing bool
	installErr string
	unknown    bool
}

// NewManager creates the production update manager. It does not make network
// requests or touch the executable until an update endpoint is called.
func NewManager(current string, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	asset := fmt.Sprintf("plans-%s-%s", runtime.GOOS, runtime.GOARCH)
	return &Manager{
		current:  current,
		asset:    asset,
		source:   newGitHubSource(),
		install:  newExecutableInstall(logger),
		logger:   logger,
		cacheTTL: defaultCacheTTL,
	}
}

// Check returns release availability and whether this particular process can
// safely install it.
func (m *Manager) Check(ctx context.Context) Report {
	m.installMu.Lock()
	installing := m.installing
	installErr := m.installErr
	unknown := m.unknown
	m.installMu.Unlock()
	if installErr != "" {
		// Recheck the executable and supervisor before offering a retry. A
		// terminal helper error can include a failed rollback, not just an
		// untouched original. Keep the warning visible without hiding a safe
		// retry behind a permanently failed check response.
		rep, _ := m.check(ctx)
		if rep.Supported && rep.UpdateAvailable {
			rep.Reason = installErr + "; you can retry the update"
		} else {
			rep.Reason = installErr + "; " + rep.Reason
		}
		return rep
	}
	if installing {
		m.cacheMu.Lock()
		latest := m.cached.version
		m.cacheMu.Unlock()
		if unknown {
			return Report{CurrentVersion: m.current, LatestVersion: latest, Status: "error",
				Reason: "update helper outcome is unknown; do not retry while it may still be running—inspect the service and update logs, then recover manually"}
		}
		return Report{CurrentVersion: m.current, LatestVersion: latest, Status: "installing",
			Reason: "an update is already in progress; if the service does not restart, check its logs before trying again"}
	}
	rep, _ := m.check(ctx)
	return rep
}

func (m *Manager) check(ctx context.Context) (Report, release) {
	rep := Report{CurrentVersion: m.current, Status: "error"}
	rel, err := m.latest(ctx)
	if errors.Is(err, ErrNoRelease) {
		rep.Status = "no_release"
		rep.Reason = ErrNoRelease.Error()
		return rep, release{}
	}
	if err != nil {
		rep.Reason = "could not check the official stable release: " + err.Error()
		return rep, release{}
	}
	rep.LatestVersion = rel.version

	latest, err := parseStableVersion(rel.version)
	if err != nil {
		rep.Reason = "official latest release has an invalid or non-stable version: " + err.Error()
		return rep, rel
	}
	current, err := parseStableVersion(m.current)
	if err != nil {
		rep.Status = "unsupported"
		rep.Reason = "this build has no unambiguous stable version; install a release build manually first"
		return rep, rel
	}

	comparison := compareVersions(current, latest)
	if comparison > 0 {
		rep.Status = "unsupported"
		rep.Reason = "the official stable release is older than this build; downgrades are refused"
		return rep, rel
	}

	supported, reason := m.install.support(ctx)
	rep.Supported = supported
	if comparison == 0 {
		rep.Status = "up_to_date"
		rep.Reason = reason
		return rep, rel
	}

	rep.UpdateAvailable = true
	if !supported {
		rep.Status = "unsupported"
		rep.Reason = reason
		return rep, rel
	}
	rep.Status = "available"
	return rep, rel
}

// Install downloads, verifies, and stages the latest stable release, then
// hands replacement/restart to the installed OS supervisor. A successful
// return means the handoff was accepted, not that the new process is healthy.
func (m *Manager) Install(ctx context.Context) (Report, error) {
	m.installMu.Lock()
	if m.installing {
		m.installMu.Unlock()
		return m.Check(ctx), ErrInstallBusy
	}
	m.installing = true
	m.installErr = ""
	m.unknown = false
	m.installMu.Unlock()

	succeeded := false
	defer func() {
		if !succeeded {
			m.installMu.Lock()
			m.installing = false
			m.installMu.Unlock()
		}
	}()

	rep, rel := m.check(ctx)
	if rep.Status != "available" || !rep.Supported {
		return rep, ErrNoUpdate
	}

	checksums, err := m.fetch(ctx, rel.checksumsURL, maxChecksums)
	if err != nil {
		return installError(rep, "could not download checksums: "+err.Error()), err
	}
	wantHash, err := checksumFor(checksums, rel.assetName)
	if err != nil {
		return installError(rep, err.Error()), err
	}

	dl, err := m.source.open(ctx, rel.assetURL, maxBinary)
	if err != nil {
		return installError(rep, "could not download update: "+err.Error()), err
	}
	defer dl.body.Close()
	if rel.assetSize <= 0 || rel.assetSize > maxBinary {
		err = fmt.Errorf("release asset size %d is invalid", rel.assetSize)
		return installError(rep, err.Error()), err
	}
	if dl.contentLength >= 0 && dl.contentLength != rel.assetSize {
		err = fmt.Errorf("download length %d does not match release metadata %d", dl.contentLength, rel.assetSize)
		return installError(rep, err.Error()), err
	}

	staged, err := m.install.stage(ctx, rel, wantHash, dl.body)
	if err != nil {
		return installError(rep, "could not stage verified update: "+err.Error()), err
	}
	completion, err := m.install.handoff(ctx, staged)
	if err != nil {
		return installError(rep, "could not schedule atomic replacement: "+err.Error()), err
	}

	succeeded = true
	go m.awaitHelper(completion)
	rep.Status = "installing"
	rep.Reason = "verified update staged; the installed service supervisor is applying it"
	m.logger.Printf("update: verified %s and handed replacement to the service supervisor", rel.version)
	return rep, nil
}

func (m *Manager) awaitHelper(completion <-chan error) {
	err, ok := <-completion
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if !ok || errors.Is(err, errHelperOutcomeUnknown) {
		m.unknown = true
		return
	}
	if err != nil {
		m.installing = false
		m.installErr = "update helper reported a terminal failure: " + err.Error()
	}
}

func installError(rep Report, reason string) Report {
	rep.Status = "error"
	rep.Reason = reason
	return rep
}

func (m *Manager) latest(ctx context.Context) (release, error) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.cached.version != "" && time.Since(m.cachedAt) < m.cacheTTL {
		return m.cached, nil
	}
	rel, err := m.source.latest(ctx, m.asset)
	if err != nil {
		return release{}, err
	}
	m.cached, m.cachedAt = rel, time.Now()
	return rel, nil
}

func (m *Manager) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	dl, err := m.source.open(ctx, url, limit)
	if err != nil {
		return nil, err
	}
	defer dl.body.Close()
	b, err := io.ReadAll(io.LimitReader(dl.body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return b, nil
}

func checksumFor(manifest []byte, assetName string) (string, error) {
	var found string
	for _, line := range strings.Split(string(manifest), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != assetName {
			continue
		}
		hash := strings.ToLower(fields[0])
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("checksums.txt has an invalid SHA256 for %s", assetName)
		}
		if found != "" && found != hash {
			return "", fmt.Errorf("checksums.txt has conflicting SHA256 entries for %s", assetName)
		}
		found = hash
	}
	if found == "" {
		return "", fmt.Errorf("checksums.txt does not contain %s", assetName)
	}
	return found, nil
}

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	launchdLabel        = "com.ayushdeolasee.plans"
	systemdUnit         = "plans.service"
	helperResultTimeout = 45 * time.Second
	maxHelperResult     = 64 << 10
)

var syncDirectoryForRollback = syncDirectory

type applyRequest struct {
	target  string
	staged  string
	backup  string
	result  string
	oldPID  int
	service serviceSpec
}

type serviceSpec struct {
	kind   string
	target string
}

type supervisor interface {
	detect(context.Context, string, int) (bool, string)
	schedule(context.Context, applyRequest) error
}

type commandRunner interface {
	run(context.Context, string, ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type osSupervisor struct {
	runner commandRunner
	mu     sync.Mutex
	found  serviceSpec
}

func newSupervisor() *osSupervisor {
	return &osSupervisor{runner: execRunner{}}
}

func (s *osSupervisor) detect(ctx context.Context, executable string, pid int) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var spec serviceSpec
	var reason string
	switch runtime.GOOS {
	case "darwin":
		spec, reason = s.detectLaunchd(ctx, executable, pid)
	case "linux":
		spec, reason = s.detectSystemd(ctx, pid)
	default:
		reason = "self-update is supported only for installed launchd and systemd services"
	}
	if spec.kind == "" {
		return false, reason
	}
	s.mu.Lock()
	s.found = spec
	s.mu.Unlock()
	return true, ""
}

func (s *osSupervisor) detectLaunchd(ctx context.Context, executable string, pid int) (serviceSpec, string) {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel)
	out, err := s.runner.run(ctx, "launchctl", "print", target)
	if err != nil {
		return serviceSpec{}, "the process is not the installed launchd service"
	}
	text := string(out)
	if parseAssignmentInt(text, "pid") != pid {
		return serviceSpec{}, "the running process is not owned by the installed launchd service"
	}
	if !assignmentEquals(text, "program", executable) {
		return serviceSpec{}, "the launchd service does not point at the running executable"
	}
	return serviceSpec{kind: "launchd", target: target}, ""
}

func (s *osSupervisor) detectSystemd(ctx context.Context, pid int) (serviceSpec, string) {
	for _, candidate := range []serviceSpec{
		{kind: "systemd-user", target: systemdUnit},
		{kind: "systemd-system", target: systemdUnit},
	} {
		args := systemctlArgs(candidate, "show", systemdUnit, "--property=MainPID", "--value")
		out, err := s.runner.run(ctx, "systemctl", args...)
		if err != nil || strings.TrimSpace(string(out)) != strconv.Itoa(pid) {
			continue
		}
		if candidate.kind == "systemd-system" && os.Geteuid() != 0 {
			return serviceSpec{}, "the system service cannot replace/restart itself without root; reinstall it as a user service or update manually"
		}
		return candidate, ""
	}
	return serviceSpec{}, "the process is not the installed systemd user or system service"
}

func (s *osSupervisor) schedule(ctx context.Context, req applyRequest) error {
	if err := validateApplyPaths(req); err != nil {
		return err
	}
	s.mu.Lock()
	req.service = s.found
	s.mu.Unlock()
	if req.service.kind == "" {
		return errors.New("service supervisor was not detected")
	}

	args := append(applyArgs(req), "--service-kind", req.service.kind, "--service-target", req.service.target)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out []byte
	var err error
	// launchd retains completed submitted jobs. Give each attempt its own
	// label so a safe retry in the same serving process can start a helper.
	attempt := fmt.Sprintf("%d-%s", req.oldPID, filepath.Base(req.staged))
	job := "plans-update-" + attempt
	switch req.service.kind {
	case "launchd":
		launchArgs := append([]string{"submit", "-l", launchdLabel + ".updater." + attempt, "--", req.target}, args...)
		out, err = s.runner.run(ctx, "launchctl", launchArgs...)
	case "systemd-user", "systemd-system":
		runArgs := []string{"--unit=" + job, "--collect", "--service-type=exec", "--"}
		if req.service.kind == "systemd-user" {
			runArgs = append([]string{"--user"}, runArgs...)
		}
		runArgs = append(runArgs, req.target)
		runArgs = append(runArgs, args...)
		out, err = s.runner.run(ctx, "systemd-run", runArgs...)
	default:
		return errors.New("unsupported service supervisor")
	}
	if err != nil {
		return fmt.Errorf("start supervised update helper: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func systemctlArgs(spec serviceSpec, args ...string) []string {
	if spec.kind == "systemd-user" {
		return append([]string{"--user"}, args...)
	}
	return args
}

func parseAssignmentInt(text, key string) int {
	prefix := key + " = "
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
			return n
		}
	}
	return 0
}

func assignmentEquals(text, key, want string) bool {
	prefix := key + " = "
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)) == want
		}
	}
	return false
}

// RunApplyHelper is the hidden, supervisor-launched half of an update. It is
// intentionally separate from the HTTP process so restarting the service does
// not kill the rollback controller with it.
func RunApplyHelper(args []string, logger *log.Logger) int {
	if logger == nil {
		logger = log.Default()
	}
	req, err := parseApplyArgs(args)
	if err != nil {
		acknowledgeApplyResult(req.result, err, logger)
		logger.Printf("update helper: %v", err)
		return 2
	}
	if err := applyAndRestart(context.Background(), req, execRunner{}, logger); err != nil {
		acknowledgeApplyResult(req.result, err, logger)
		logger.Printf("update helper: %v", err)
		return 1
	}
	acknowledgeApplyResult(req.result, nil, logger)
	return 0
}

func parseApplyArgs(args []string) (applyRequest, error) {
	var req applyRequest
	for len(args) > 0 {
		if len(args) < 2 {
			return req, errors.New("incomplete update helper arguments")
		}
		key, value := args[0], args[1]
		args = args[2:]
		switch key {
		case "--target":
			req.target = value
		case "--staged":
			req.staged = value
		case "--backup":
			req.backup = value
		case "--result":
			req.result = value
		case "--old-pid":
			req.oldPID, _ = strconv.Atoi(value)
		case "--service-kind":
			req.service.kind = value
		case "--service-target":
			req.service.target = value
		default:
			return req, fmt.Errorf("unknown update helper argument %q", key)
		}
	}
	if err := validateApplyPaths(req); err != nil {
		return req, err
	}
	if req.oldPID == os.Getpid() {
		return req, errors.New("update helper must be separate from the serving process")
	}
	switch req.service.kind {
	case "launchd", "systemd-user", "systemd-system":
	default:
		return req, errors.New("invalid service supervisor")
	}
	if req.service.target == "" {
		return req, errors.New("missing service target")
	}
	return req, nil
}

func acknowledgeApplyResult(path string, result error, logger *log.Logger) {
	if path == "" {
		return
	}
	contents := "ok\n"
	if result != nil {
		contents = "error\n" + result.Error()
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, err = f.WriteString(contents); err == nil {
			err = f.Sync()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		err = syncDirectory(filepath.Dir(path))
	}
	if err != nil {
		_ = os.Remove(tmp)
		logger.Printf("update helper: could not acknowledge completion: %v", err)
	}
}

func waitForApplyResult(path string, timeout time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		deadline := time.NewTimer(timeout)
		defer deadline.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			contents, err := os.ReadFile(path)
			if err == nil {
				_ = os.Remove(path)
				if len(contents) > maxHelperResult {
					done <- fmt.Errorf("%w: acknowledgement exceeds size limit", errHelperOutcomeUnknown)
					return
				}
				text := string(contents)
				switch {
				case text == "ok\n":
					done <- nil
				case strings.HasPrefix(text, "error\n"):
					done <- errors.New(strings.TrimSpace(strings.TrimPrefix(text, "error\n")))
				default:
					done <- fmt.Errorf("%w: malformed acknowledgement", errHelperOutcomeUnknown)
				}
				return
			}
			if !errors.Is(err, os.ErrNotExist) {
				done <- fmt.Errorf("%w: cannot read acknowledgement: %v", errHelperOutcomeUnknown, err)
				return
			}
			select {
			case <-deadline.C:
				done <- fmt.Errorf("%w after %s; helper result path is %s", errHelperOutcomeUnknown, timeout, path)
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

func applyAndRestart(ctx context.Context, req applyRequest, runner commandRunner, logger *log.Logger) error {
	// Give the POST response a chance to flush before the supervisor replaces
	// and restarts the serving process.
	timer := time.NewTimer(750 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
	}

	if err := validateApplyPaths(req); err != nil {
		return err
	}
	oldInfo, err := os.Lstat(req.target)
	if err != nil {
		return fmt.Errorf("original executable is unavailable: %w", err)
	}
	if !oldInfo.Mode().IsRegular() {
		return errors.New("original executable is not a regular file")
	}
	newInfo, err := os.Lstat(req.staged)
	if err != nil {
		return fmt.Errorf("staged executable is unavailable: %w", err)
	}
	if !newInfo.Mode().IsRegular() {
		return errors.New("staged executable is not a regular file")
	}
	if oldInfo.Mode().Perm() != newInfo.Mode().Perm() {
		return errors.New("staged executable permissions do not match the original")
	}

	// Keep the live path present at all times: hard-link the original inode
	// for rollback, then atomically replace the directory entry. Two renames
	// would leave a crash window where the supervisor has no executable.
	if err := os.Remove(req.backup); err != nil {
		return fmt.Errorf("prepare rollback path: %w", err)
	}
	if err := os.Link(req.target, req.backup); err != nil {
		return fmt.Errorf("preserve original executable: %w", err)
	}
	if err := syncDirectory(filepath.Dir(req.target)); err != nil {
		_ = os.Remove(req.backup)
		return fmt.Errorf("sync rollback executable: %w", err)
	}
	if err := os.Rename(req.staged, req.target); err != nil {
		_ = os.Remove(req.backup)
		return fmt.Errorf("activate staged executable: %w", err)
	}
	if err := syncDirectory(filepath.Dir(req.target)); err != nil {
		return rollback(req, runner, fmt.Errorf("sync executable replacement: %w", err))
	}

	if err := restartAndVerify(ctx, req, runner); err != nil {
		return rollback(req, runner, err)
	}
	// A new PID proves supervisor activation, not application health. Keep
	// the original for manual recovery even after activation; the browser
	// independently confirms the new running version through /healthz.
	logger.Printf("update helper: new service is active; previous executable retained at %s", req.backup)
	_ = syncDirectory(filepath.Dir(req.target))
	return nil
}

func rollback(req applyRequest, runner commandRunner, updateErr error) error {
	// Restore the original in one atomic operation as well.
	if err := os.Rename(req.backup, req.target); err != nil {
		return fmt.Errorf("%v; rollback could not restore original executable: %w", updateErr, err)
	}
	syncErr := syncDirectoryForRollback(filepath.Dir(req.target))
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := restartService(rollbackCtx, req.service, runner); err != nil {
		if syncErr != nil {
			return fmt.Errorf("%v; original executable restored but directory sync failed: %v; restart failed: %w", updateErr, syncErr, err)
		}
		return fmt.Errorf("%v; original executable restored but restart failed: %w", updateErr, err)
	}
	_ = os.Remove(req.staged)
	if syncErr != nil {
		return fmt.Errorf("%v; original executable restored and restarted but directory sync failed: %w", updateErr, syncErr)
	}
	return fmt.Errorf("%v; original executable restored and restarted", updateErr)
}

func restartAndVerify(ctx context.Context, req applyRequest, runner commandRunner) error {
	restartCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := restartService(restartCtx, req.service, runner); err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		pid, running := servicePID(restartCtx, req.service, runner)
		if running && pid > 0 && pid != req.oldPID {
			return nil
		}
		select {
		case <-restartCtx.Done():
			return errors.New("updated service did not become active with a new process")
		case <-ticker.C:
		}
	}
}

func restartService(ctx context.Context, spec serviceSpec, runner commandRunner) error {
	var name string
	var args []string
	switch spec.kind {
	case "launchd":
		name, args = "launchctl", []string{"kickstart", "-k", spec.target}
	case "systemd-user", "systemd-system":
		name, args = "systemctl", systemctlArgs(spec, "restart", spec.target)
	default:
		return errors.New("unsupported service supervisor")
	}
	out, err := runner.run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("restart %s: %s (%w)", spec.kind, strings.TrimSpace(string(out)), err)
	}
	return nil
}

func servicePID(ctx context.Context, spec serviceSpec, runner commandRunner) (int, bool) {
	switch spec.kind {
	case "launchd":
		out, err := runner.run(ctx, "launchctl", "print", spec.target)
		if err != nil {
			return 0, false
		}
		text := string(out)
		return parseAssignmentInt(text, "pid"), strings.Contains(text, "state = running")
	case "systemd-user", "systemd-system":
		active, err := runner.run(ctx, "systemctl", systemctlArgs(spec, "is-active", spec.target)...)
		if err != nil || strings.TrimSpace(string(active)) != "active" {
			return 0, false
		}
		out, err := runner.run(ctx, "systemctl", systemctlArgs(spec, "show", spec.target, "--property=MainPID", "--value")...)
		if err != nil {
			return 0, false
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
		return pid, pid > 0
	default:
		return 0, false
	}
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

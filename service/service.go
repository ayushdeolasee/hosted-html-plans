package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/ayushdeolasee/hosted-html-plans/server"
)

// launchdLabel is the launchd job label / plist basename, matching
// plan.html §5: ~/Library/LaunchAgents/com.ayushdeolasee.plans.plist.
const launchdLabel = "com.ayushdeolasee.plans"

// systemdUnitName is the systemd unit file name, for both the system and
// user install modes.
const systemdUnitName = "plans.service"

// ErrNeedsRoot is returned by Install/Uninstall when a system-level
// systemd unit is requested but the process isn't running as root. The
// caller has already printed the exact commands to run; this is a signal
// for a non-zero exit code, not a "something went wrong" error.
var ErrNeedsRoot = errors.New("root privileges required for a system-level systemd unit")

// Options configures an install/uninstall/status call.
type Options struct {
	// UserMode installs/queries a user-level systemd unit
	// (~/.config/systemd/user/plans.service, systemctl --user) instead of
	// the system unit. macOS ignores this — LaunchAgents are always
	// per-user.
	UserMode bool
	// DryRun renders the plist/unit and prints the commands that would run,
	// without writing any files or invoking launchctl/systemctl. Set via
	// --print or the PLANS_SERVICE_DRYRUN env var.
	DryRun bool
}

// BinaryPath resolves the absolute, symlink-resolved path to the currently
// running binary — what gets written into ProgramArguments / ExecStart so
// the installed service always points at a concrete file.
func BinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %s: %w", exe, err)
	}
	return real, nil
}

// ---- template rendering ----------------------------------------------

type launchdData struct {
	Label      string
	BinaryPath string
	LogPath    string
}

func renderLaunchd(d launchdData) (string, error) {
	tmpl, err := template.New("launchd").Parse(launchdTemplateSrc)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

type systemdData struct {
	BinaryPath string
	// User is left empty for a user-level unit (it already runs as the
	// invoking user); set for the system unit so it runs unprivileged
	// even with nobody logged in.
	User     string
	WantedBy string
}

func renderSystemd(d systemdData) (string, error) {
	tmpl, err := template.New("systemd").Parse(systemdTemplateSrc)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ---- paths --------------------------------------------------------------

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func launchdLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "plans.log"), nil
}

func systemdSystemUnitPath() string {
	return filepath.Join("/etc", "systemd", "system", systemdUnitName)
}

func systemdUserUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}

// invokingUser returns the username the system-level systemd unit should
// run as. If we're already elevated via sudo, SUDO_USER tells us who asked
// for the elevation; otherwise fall back to the current user.
func invokingUser() (string, error) {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u, nil
	}
	cur, err := user.Current()
	if err != nil {
		return "", err
	}
	return cur.Username, nil
}

// ---- install --------------------------------------------------------------

// Install ensures the config exists and registers the service with the
// OS's supervisor (launchd on macOS, systemd on Linux), per plan.html §5.
// It returns human-readable output describing what happened (or, in
// DryRun mode, what would happen) for the caller to print.
func Install(opts Options) (string, error) {
	// Ensure config + data dir exist first (reuses server package's
	// generation logic so the service has something to run against).
	if _, err := server.LoadConfig(); err != nil {
		return "", fmt.Errorf("ensure config: %w", err)
	}
	dataDir, err := server.DataDir()
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", fmt.Errorf("create data dir %s: %w", dataDir, err)
	}

	bin, err := BinaryPath()
	if err != nil {
		return "", err
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(bin, opts)
	case "linux":
		if opts.UserMode {
			return installSystemdUser(bin, opts)
		}
		return installSystemdSystem(bin, opts)
	default:
		return "", fmt.Errorf("plans service install is not supported on %s", runtime.GOOS)
	}
}

func installLaunchd(bin string, opts Options) (string, error) {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return "", err
	}
	logPath, err := launchdLogPath()
	if err != nil {
		return "", err
	}
	rendered, err := renderLaunchd(launchdData{Label: launchdLabel, BinaryPath: bin, LogPath: logPath})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "launchd LaunchAgent: %s\n\n%s\n", plistPath, rendered)

	uid := os.Getuid()
	guiTarget := fmt.Sprintf("gui/%d", uid)
	bootstrapCmd := []string{"launchctl", "bootstrap", guiTarget, plistPath}
	kickstartCmd := []string{"launchctl", "kickstart", "-k", fmt.Sprintf("%s/%s", guiTarget, launchdLabel)}

	if opts.DryRun || dryRunEnv() {
		fmt.Fprintf(&b, "[dry-run] would write plist to %s\n", plistPath)
		fmt.Fprintf(&b, "[dry-run] would ensure log dir for %s\n", logPath)
		fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(bootstrapCmd, " "))
		fmt.Fprintf(&b, "[dry-run] (falls back to `launchctl load -w %s` on older macOS)\n", plistPath)
		fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(kickstartCmd, " "))
		return b.String(), nil
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(plistPath, []byte(rendered), 0o644); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}

	if out, err := exec.Command(bootstrapCmd[0], bootstrapCmd[1:]...).CombinedOutput(); err != nil {
		// Older macOS: launchctl bootstrap can fail (e.g. "Bootstrap
		// failed: 5: Input/output error" on some versions); fall back to
		// the legacy `launchctl load -w`.
		loadOut, loadErr := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput()
		if loadErr != nil {
			return b.String(), fmt.Errorf("launchctl bootstrap failed: %s (%w); legacy `launchctl load -w` fallback also failed: %s (%w)",
				strings.TrimSpace(string(out)), err, strings.TrimSpace(string(loadOut)), loadErr)
		}
		fmt.Fprintf(&b, "launchctl bootstrap failed (%s); used legacy `launchctl load -w` instead\n", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(kickstartCmd[0], kickstartCmd[1:]...).CombinedOutput(); err != nil {
		fmt.Fprintf(&b, "warning: launchctl kickstart failed: %s (%v)\n", strings.TrimSpace(string(out)), err)
	}

	fmt.Fprintf(&b, "installed and started. Logs: %s\n", logPath)
	return b.String(), nil
}

func installSystemdUser(bin string, opts Options) (string, error) {
	unitPath, err := systemdUserUnitPath()
	if err != nil {
		return "", err
	}
	rendered, err := renderSystemd(systemdData{BinaryPath: bin, User: "", WantedBy: "default.target"})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "systemd user unit: %s\n\n%s\n", unitPath, rendered)

	cmds := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", systemdUnitName},
	}

	if opts.DryRun || dryRunEnv() {
		fmt.Fprintf(&b, "[dry-run] would write unit to %s\n", unitPath)
		for _, c := range cmds {
			fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(c, " "))
		}
		return b.String(), nil
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(unitPath, []byte(rendered), 0o644); err != nil {
		return "", err
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return b.String(), fmt.Errorf("%s: %s (%w)", strings.Join(c, " "), strings.TrimSpace(string(out)), err)
		}
	}

	fmt.Fprintf(&b, "installed and started (user unit). Logs: journalctl --user -u %s\n", systemdUnitName)
	return b.String(), nil
}

func installSystemdSystem(bin string, opts Options) (string, error) {
	unitPath := systemdSystemUnitPath()
	runAsUser, err := invokingUser()
	if err != nil {
		return "", err
	}
	rendered, err := renderSystemd(systemdData{BinaryPath: bin, User: runAsUser, WantedBy: "multi-user.target"})
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "systemd system unit: %s (runs as user %q)\n\n%s\n", unitPath, runAsUser, rendered)

	cmds := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", systemdUnitName},
	}

	if opts.DryRun || dryRunEnv() {
		fmt.Fprintf(&b, "[dry-run] would write unit to %s\n", unitPath)
		for _, c := range cmds {
			fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(c, " "))
		}
		return b.String(), nil
	}

	if os.Geteuid() != 0 {
		// Sudo handling choice: we never shell out to `sudo` ourselves
		// (that would either hang on a password prompt in a non-interactive
		// context, like an agent or CI, or silently fail). Instead we print
		// the exact commands the user can copy-paste, and point at the two
		// escape hatches: re-run the whole command under sudo, or use
		// --user to avoid root entirely.
		fmt.Fprintf(&b, "root privileges are required to install a system-level systemd unit.\n")
		fmt.Fprintf(&b, "Run the following to finish installation:\n\n")
		fmt.Fprintf(&b, "  sudo tee %s > /dev/null <<'EOF'\n%s\nEOF\n", unitPath, rendered)
		for _, c := range cmds {
			fmt.Fprintf(&b, "  sudo %s\n", strings.Join(c, " "))
		}
		fmt.Fprintf(&b, "\nOr re-run this command with sudo:\n  sudo %s service install\n", bin)
		fmt.Fprintf(&b, "\nOr install a user-level unit instead (no root required):\n  %s service install --user\n", bin)
		return b.String(), ErrNeedsRoot
	}

	if err := os.WriteFile(unitPath, []byte(rendered), 0o644); err != nil {
		return "", err
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return b.String(), fmt.Errorf("%s: %s (%w)", strings.Join(c, " "), strings.TrimSpace(string(out)), err)
		}
	}

	fmt.Fprintf(&b, "installed and started. Logs: journalctl -u %s\n", systemdUnitName)
	return b.String(), nil
}

func dryRunEnv() bool {
	return os.Getenv("PLANS_SERVICE_DRYRUN") != ""
}

// ---- uninstall --------------------------------------------------------------

// Uninstall stops and removes the service registration. It is idempotent:
// running it when nothing is installed is not an error.
func Uninstall(opts Options) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd(opts)
	case "linux":
		if opts.UserMode {
			return uninstallSystemdUser(opts)
		}
		return uninstallSystemdSystem(opts)
	default:
		return "", fmt.Errorf("plans service uninstall is not supported on %s", runtime.GOOS)
	}
}

func uninstallLaunchd(opts Options) (string, error) {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return "", err
	}
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, launchdLabel)
	bootoutCmd := []string{"launchctl", "bootout", target}

	var b strings.Builder
	if opts.DryRun || dryRunEnv() {
		fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(bootoutCmd, " "))
		fmt.Fprintf(&b, "[dry-run] would remove: %s\n", plistPath)
		return b.String(), nil
	}

	// Idempotent: ignore errors here (e.g. "no such process" if it wasn't
	// loaded) and fall back to the legacy unload, also best-effort.
	if _, err := exec.Command(bootoutCmd[0], bootoutCmd[1:]...).CombinedOutput(); err != nil {
		_, _ = exec.Command("launchctl", "unload", plistPath).CombinedOutput()
	}

	if _, err := os.Stat(plistPath); err == nil {
		if err := os.Remove(plistPath); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "removed %s\n", plistPath)
	} else {
		fmt.Fprintf(&b, "%s was not present (already uninstalled)\n", plistPath)
	}
	fmt.Fprintf(&b, "service stopped and unregistered\n")
	return b.String(), nil
}

func uninstallSystemdUser(opts Options) (string, error) {
	unitPath, err := systemdUserUnitPath()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	cmds := [][]string{
		{"systemctl", "--user", "disable", "--now", systemdUnitName},
		{"systemctl", "--user", "daemon-reload"},
	}
	if opts.DryRun || dryRunEnv() {
		for _, c := range cmds {
			fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(c, " "))
		}
		fmt.Fprintf(&b, "[dry-run] would remove: %s\n", unitPath)
		return b.String(), nil
	}
	for _, c := range cmds {
		// Idempotent: best-effort, don't fail if it wasn't installed.
		_, _ = exec.Command(c[0], c[1:]...).CombinedOutput()
	}
	if _, err := os.Stat(unitPath); err == nil {
		if err := os.Remove(unitPath); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "removed %s\n", unitPath)
	} else {
		fmt.Fprintf(&b, "%s was not present (already uninstalled)\n", unitPath)
	}
	fmt.Fprintf(&b, "service stopped and unregistered (user unit)\n")
	return b.String(), nil
}

func uninstallSystemdSystem(opts Options) (string, error) {
	unitPath := systemdSystemUnitPath()
	var b strings.Builder
	cmds := [][]string{
		{"systemctl", "disable", "--now", systemdUnitName},
		{"systemctl", "daemon-reload"},
	}
	if opts.DryRun || dryRunEnv() {
		for _, c := range cmds {
			fmt.Fprintf(&b, "[dry-run] would run: %s\n", strings.Join(c, " "))
		}
		fmt.Fprintf(&b, "[dry-run] would remove: %s\n", unitPath)
		return b.String(), nil
	}
	if os.Geteuid() != 0 {
		fmt.Fprintf(&b, "root privileges are required to remove the system-level systemd unit.\n")
		fmt.Fprintf(&b, "Run the following to finish uninstalling:\n\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "  sudo %s\n", strings.Join(c, " "))
		}
		fmt.Fprintf(&b, "  sudo rm -f %s\n", unitPath)
		return b.String(), ErrNeedsRoot
	}
	for _, c := range cmds {
		_, _ = exec.Command(c[0], c[1:]...).CombinedOutput()
	}
	if _, err := os.Stat(unitPath); err == nil {
		if err := os.Remove(unitPath); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "removed %s\n", unitPath)
	} else {
		fmt.Fprintf(&b, "%s was not present (already uninstalled)\n", unitPath)
	}
	fmt.Fprintf(&b, "service stopped and unregistered\n")
	return b.String(), nil
}

// ---- status --------------------------------------------------------------

// StatusReport is the assembled answer to `plans service status`.
type StatusReport struct {
	OS         string
	Mode       string
	Registered bool
	Running    bool
	RawStatus  string

	ConfigPath string
	DataDir    string
	LANListen  string

	HealthzURL string
	HealthzOK  bool
	HealthzErr string

	// TailnetAuth is the human-readable tailnet auth/connection state, derived
	// from config plus the snapshot the running server persists to
	// <data-dir>/tsnet-status.json (plan.html §5 first-run flow).
	TailnetAuth string
}

// Status reports whether the service is registered/running with the OS
// supervisor, whether /healthz answers on the configured LAN port, and
// where the config/data live.
func Status(opts Options) (StatusReport, error) {
	rep := StatusReport{OS: runtime.GOOS}

	if p, err := server.ConfigPath(); err == nil {
		rep.ConfigPath = p
	}
	if d, err := server.DataDir(); err == nil {
		rep.DataDir = d
	}

	cfg, cfgErr := server.LoadConfig()
	if cfgErr == nil {
		rep.LANListen = cfg.LANListen
	}
	rep.TailnetAuth = tailnetAuthLine(cfg, cfgErr, rep.DataDir)

	switch runtime.GOOS {
	case "darwin":
		statusLaunchd(&rep)
	case "linux":
		statusSystemd(&rep, opts.UserMode)
	default:
		rep.Mode = "unsupported"
	}

	checkHealthz(&rep, cfg.LANListen)

	return rep, nil
}

// tailnetAuthLine renders the tailnet auth/connection state for the status
// report. It reads the snapshot the running server persists; when tailscale
// is disabled it reports so without touching any tsnet state.
func tailnetAuthLine(cfg server.Config, cfgErr error, dataDir string) string {
	if cfgErr != nil {
		return "unknown (config could not be read)"
	}
	if !cfg.TailscaleEnabled {
		return "disabled (tailscale_enabled=false)"
	}
	if dataDir == "" {
		return "enabled; state unknown (data dir unresolved)"
	}
	st, err := server.ReadTailnetState(dataDir)
	if err != nil {
		return "enabled; not yet started (no state recorded — start the service once to authenticate)"
	}
	switch st.State {
	case server.TailnetStateAuthenticated:
		if st.NodeName != "" {
			return fmt.Sprintf("authenticated as %s", st.NodeName)
		}
		return "authenticated"
	case server.TailnetStateWaitingAuth:
		if st.AuthURL != "" {
			return fmt.Sprintf("waiting for auth — visit %s", st.AuthURL)
		}
		return "waiting for auth (URL not captured yet)"
	case server.TailnetStateStarting:
		if st.Err != "" {
			return fmt.Sprintf("enabled; starting (last error: %s)", st.Err)
		}
		return "enabled; starting up"
	default:
		if st.State == "" {
			return "enabled; state unknown"
		}
		return fmt.Sprintf("enabled; state=%s", st.State)
	}
}

func statusLaunchd(rep *StatusReport) {
	rep.Mode = "launchd"
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, launchdLabel)
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	rep.RawStatus = strings.TrimSpace(string(out))
	if err == nil {
		rep.Registered = true
		rep.Running = strings.Contains(rep.RawStatus, "state = running")
	}
}

func statusSystemd(rep *StatusReport, userMode bool) {
	rep.Mode = "systemd-system"
	activeArgs := []string{"is-active", systemdUnitName}
	enabledArgs := []string{"is-enabled", systemdUnitName}
	if userMode {
		rep.Mode = "systemd-user"
		activeArgs = append([]string{"--user"}, activeArgs...)
		enabledArgs = append([]string{"--user"}, enabledArgs...)
	}

	activeOut, _ := exec.Command("systemctl", activeArgs...).CombinedOutput()
	activeStatus := strings.TrimSpace(string(activeOut))
	rep.Running = activeStatus == "active"

	enabledOut, _ := exec.Command("systemctl", enabledArgs...).CombinedOutput()
	enabledStatus := strings.TrimSpace(string(enabledOut))
	rep.Registered = enabledStatus == "enabled" || rep.Running

	rep.RawStatus = fmt.Sprintf("is-active: %s\nis-enabled: %s", activeStatus, enabledStatus)
}

func checkHealthz(rep *StatusReport, lanListen string) {
	if lanListen == "" || lanListen == "off" {
		rep.HealthzErr = "lan_listen is off in config; nothing to check on the LAN listener"
		return
	}
	_, port, err := net.SplitHostPort(lanListen)
	if err != nil {
		rep.HealthzErr = fmt.Sprintf("could not parse port from lan_listen %q: %v", lanListen, err)
		return
	}
	rep.HealthzURL = fmt.Sprintf("http://127.0.0.1:%s/healthz", port)

	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(rep.HealthzURL)
	if err != nil {
		rep.HealthzErr = err.Error()
		return
	}
	defer resp.Body.Close()
	rep.HealthzOK = resp.StatusCode == http.StatusOK
	if !rep.HealthzOK {
		rep.HealthzErr = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
}

// PrintStatus writes a human-readable rendering of a StatusReport.
func PrintStatus(w io.Writer, rep StatusReport) {
	fmt.Fprintf(w, "OS:              %s\n", rep.OS)
	fmt.Fprintf(w, "Service mode:    %s\n", rep.Mode)
	fmt.Fprintf(w, "Registered:      %t\n", rep.Registered)
	fmt.Fprintf(w, "Running:         %t\n", rep.Running)
	if rep.RawStatus != "" {
		fmt.Fprintf(w, "\n--- supervisor status ---\n%s\n", rep.RawStatus)
	}
	fmt.Fprintf(w, "\nConfig path:     %s\n", rep.ConfigPath)
	fmt.Fprintf(w, "Data dir:        %s\n", rep.DataDir)
	fmt.Fprintf(w, "lan_listen:      %s\n", rep.LANListen)
	if rep.HealthzURL != "" {
		fmt.Fprintf(w, "Healthz:         %s -> %t\n", rep.HealthzURL, rep.HealthzOK)
	}
	if rep.HealthzErr != "" {
		fmt.Fprintf(w, "Healthz note:    %s\n", rep.HealthzErr)
	}
	fmt.Fprintf(w, "Tailnet auth:    %s\n", rep.TailnetAuth)
}

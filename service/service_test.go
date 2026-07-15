package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderLaunchdPlist(t *testing.T) {
	out, err := renderLaunchd(launchdData{
		Label:      "com.ayushdeolasee.plans",
		BinaryPath: "/usr/local/bin/plans",
		LogPath:    "/Users/ayush/Library/Logs/plans.log",
	})
	if err != nil {
		t.Fatalf("renderLaunchd: %v", err)
	}

	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.ayushdeolasee.plans</string>",
		"<string>/usr/local/bin/plans</string>",
		"<string>run</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>KeepAlive</key>",
		"<key>StandardOutPath</key>",
		"<string>/Users/ayush/Library/Logs/plans.log</string>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered plist missing %q\n---\n%s", want, out)
		}
	}

	if !strings.HasPrefix(out, "<?xml") {
		t.Errorf("rendered plist should start with the XML declaration, got:\n%s", out[:min(40, len(out))])
	}
}

func TestRenderLaunchdPlistValidatesOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is only available on macOS")
	}
	out, err := renderLaunchd(launchdData{
		Label:      "com.ayushdeolasee.plans",
		BinaryPath: "/usr/local/bin/plans",
		LogPath:    filepath.Join(t.TempDir(), "plans.log"),
	})
	if err != nil {
		t.Fatalf("renderLaunchd: %v", err)
	}
	path := filepath.Join(t.TempDir(), "rendered.plist")
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("write rendered plist: %v", err)
	}
	if err := runPlutilLint(path); err != nil {
		t.Fatalf("plutil considers the rendered plist invalid: %v\n---\n%s", err, out)
	}
}

func TestRenderSystemdSystemUnit(t *testing.T) {
	out, err := renderSystemd(systemdData{
		BinaryPath: "/opt/plans/plans",
		User:       "ayush",
		WantedBy:   "multi-user.target",
	})
	if err != nil {
		t.Fatalf("renderSystemd: %v", err)
	}
	for _, want := range []string{
		"User=ayush",
		"ExecStart=/opt/plans/plans run",
		"Restart=on-failure",
		"MemoryMax=128M",
		"CPUQuota=50%",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered system unit missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderSystemdUserUnitOmitsUserLine(t *testing.T) {
	out, err := renderSystemd(systemdData{
		BinaryPath: "/opt/plans/plans",
		User:       "",
		WantedBy:   "default.target",
	})
	if err != nil {
		t.Fatalf("renderSystemd: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "User=") {
			t.Errorf("user-level unit should not set a User= directive, found line %q in:\n%s", line, out)
		}
	}
	if !strings.Contains(out, "WantedBy=default.target") {
		t.Errorf("rendered user unit missing WantedBy=default.target\n---\n%s", out)
	}
	if !strings.Contains(out, "ExecStart=/opt/plans/plans run") {
		t.Errorf("rendered user unit missing ExecStart\n---\n%s", out)
	}
}

func TestInstallDryRunDoesNotTouchDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLANS_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("PLANS_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("HOME", dir) // keep plist/unit paths sandboxed even though DryRun shouldn't write them

	out, err := Install(Options{DryRun: true})
	if err != nil {
		t.Fatalf("Install (dry-run): %v", err)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Errorf("expected dry-run output to be marked, got:\n%s", out)
	}

	// Config should still have been generated (Install ensures it exists
	// regardless of dry-run), but no plist/unit file should appear.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Errorf("expected config.json to be generated even in dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Library", "LaunchAgents")); err == nil {
		t.Errorf("dry-run should not have created LaunchAgents dir")
	}
}

// runPlutilLint shells out to macOS's plutil to confirm the rendered plist
// is well-formed XML property-list syntax, not just "contains the right
// substrings".
func runPlutilLint(path string) error {
	out, err := exec.Command("plutil", "-lint", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

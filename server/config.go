// Package server implements the hosted-html-plans core server: config,
// a versioned file store, and the HTTP routers.
package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the on-disk configuration, stored as JSON at the config path.
type Config struct {
	// LANListen is the address for the plain-TCP LAN listener.
	// "0.0.0.0:8080" (default), "127.0.0.1:8080", a custom addr, or "off".
	LANListen string `json:"lan_listen"`
	// TailscaleEnabled carries the tsnet toggle. tsnet itself is a later
	// phase; the field is persisted now so config format stays stable.
	TailscaleEnabled bool `json:"tailscale_enabled"`
	// Hostname is the tailnet node name (also used to shape share URLs).
	Hostname string `json:"hostname"`
	// TailnetDomain, when known (e.g. "example.ts.net"), lets the server
	// emit absolute tailnet/share URLs. Empty until the tsnet phase fills it.
	TailnetDomain string `json:"tailnet_domain,omitempty"`
}

// DefaultConfig returns the config used on first run.
func DefaultConfig() Config {
	return Config{
		LANListen:        "0.0.0.0:8080",
		TailscaleEnabled: true,
		Hostname:         "plans",
	}
}

// ConfigPath returns the config.json location, honoring the PLANS_CONFIG
// override. Default is ~/.config/plans/config.json on both OSes.
func ConfigPath() (string, error) {
	if p := os.Getenv("PLANS_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "plans", "config.json"), nil
}

// DataDir returns the data directory, honoring the PLANS_DATA_DIR override.
// Default: ~/Library/Application Support/plans (macOS),
// ~/.local/share/plans (Linux/XDG, respecting XDG_DATA_HOME).
func DataDir() (string, error) {
	if d := os.Getenv("PLANS_DATA_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "plans"), nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "plans"), nil
	}
	return filepath.Join(home, ".local", "share", "plans"), nil
}

// LoadConfig reads the config, generating it with defaults on first run.
func LoadConfig() (Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg := DefaultConfig()
		if werr := SaveConfig(cfg); werr != nil {
			return Config{}, werr
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, err
	}
	// Start from defaults so unspecified fields keep sane values.
	cfg := DefaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig atomically writes the config to the config path.
func SaveConfig(cfg Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0o644)
}

// atomicWrite writes data to a temp file in the same directory and renames
// it over path, so readers never observe a partial file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

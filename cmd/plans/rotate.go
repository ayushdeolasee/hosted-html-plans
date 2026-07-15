package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// maxLogSize is the rotate-on-startup threshold from plan.html §7 ("macOS
// log file is size-capped by a built-in rotate-on-startup check"), ~10MB.
const maxLogSize = 10 * 1024 * 1024

// rotateLogIfNeeded implements that check for the macOS launchd log file
// (~/Library/Logs/plans.log, per plan.html §5). It's called at the top of
// `plans run` (both the foreground dev path and the launchd-supervised
// path share this entrypoint). If the file is over maxLogSize, it's moved
// aside to plans.log.1 (overwriting any previous .1) so the next line
// written starts a fresh file.
//
// Linux is a no-op: systemd unit logs go to the journal, which handles its
// own rotation (journalctl -u plans). A missing log file (e.g. a
// foreground dev run before the service has ever logged anything) is also
// a no-op — nothing to rotate.
func rotateLogIfNeeded() {
	if runtime.GOOS != "darwin" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(home, "Library", "Logs", "plans.log")
	info, err := os.Stat(logPath)
	if err != nil {
		return
	}
	if info.Size() <= maxLogSize {
		return
	}
	rotated := logPath + ".1"
	if err := os.Rename(logPath, rotated); err != nil {
		log.Printf("log rotate: %v", err)
	}
}

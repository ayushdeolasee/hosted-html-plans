package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetryRejectsReplacedExecutableUntilServerRestarts(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "plans")
	if err := os.WriteFile(target, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, "replacement")
	if err := os.WriteFile(staged, []byte("replacement"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, target); err != nil {
		t.Fatal(err)
	}
	installer := &executableInstall{target: target, original: original}
	ok, reason := installer.support(context.Background())
	if ok || !strings.Contains(reason, "changed on disk") {
		t.Fatalf("a surviving old process must not update a different executable: %v, %s", ok, reason)
	}
}

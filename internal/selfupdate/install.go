package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type executableInstall struct {
	target     string
	original   os.FileInfo
	resolveErr error
	supervisor supervisor
	logger     *log.Logger
}

func newExecutableInstall(logger *log.Logger) *executableInstall {
	target, err := os.Executable()
	if err == nil {
		target, err = filepath.EvalSymlinks(target)
	}
	if err == nil {
		target, err = filepath.Abs(target)
	}
	var original os.FileInfo
	if err == nil {
		original, err = os.Stat(target)
	}
	return &executableInstall{
		target:     target,
		original:   original,
		resolveErr: err,
		supervisor: newSupervisor(),
		logger:     logger,
	}
}

func (i *executableInstall) support(ctx context.Context) (bool, string) {
	if i.resolveErr != nil {
		return false, "cannot resolve the running executable: " + i.resolveErr.Error()
	}
	info, err := os.Lstat(i.target)
	if err != nil {
		return false, "cannot inspect the running executable: " + err.Error()
	}
	if !info.Mode().IsRegular() {
		return false, "the running executable is not a regular file"
	}
	if i.original != nil && !os.SameFile(i.original, info) {
		return false, "the executable changed on disk; restart the service manually before trying another update"
	}
	if info.Mode().Perm()&0o111 == 0 {
		return false, "the running file is not executable"
	}

	probe, err := os.CreateTemp(filepath.Dir(i.target), ".plans-update-write-test-*")
	if err != nil {
		return false, "the executable directory is not writable: " + err.Error()
	}
	probeName := probe.Name()
	if closeErr := probe.Close(); closeErr != nil {
		_ = os.Remove(probeName)
		return false, "cannot safely write beside the executable: " + closeErr.Error()
	}
	if err := os.Remove(probeName); err != nil {
		return false, "cannot clean up an executable-directory write test: " + err.Error()
	}

	ok, reason := i.supervisor.detect(ctx, i.target, os.Getpid())
	if !ok {
		return false, reason
	}
	return true, ""
}

func (i *executableInstall) stage(ctx context.Context, rel release, wantHash string, body io.Reader) (staged string, err error) {
	info, err := os.Stat(i.target)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(i.target), ".plans-update-*")
	if err != nil {
		return "", err
	}
	staged = f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(staged)
		}
	}()

	h := sha256.New()
	limited := &io.LimitedReader{R: body, N: maxBinary + 1}
	written, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		return "", err
	}
	if written > maxBinary {
		return "", fmt.Errorf("update exceeds %d bytes", maxBinary)
	}
	if written != rel.assetSize {
		return "", fmt.Errorf("downloaded %d bytes; release metadata requires %d", written, rel.assetSize)
	}
	gotHash := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(gotHash, wantHash) {
		return "", fmt.Errorf("SHA256 mismatch: got %s, want %s", gotHash, wantHash)
	}
	if err := f.Chmod(info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("preserve executable permissions: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync staged executable: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close staged executable: %w", err)
	}
	return staged, nil
}

func (i *executableInstall) handoff(ctx context.Context, staged string) (<-chan error, error) {
	backup, err := os.CreateTemp(filepath.Dir(i.target), ".plans-backup-*")
	if err != nil {
		_ = os.Remove(staged)
		return nil, fmt.Errorf("reserve rollback file: %w", err)
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupPath)
		_ = os.Remove(staged)
		return nil, err
	}
	result, err := os.CreateTemp(filepath.Dir(i.target), ".plans-update-result-*")
	if err != nil {
		_ = os.Remove(backupPath)
		_ = os.Remove(staged)
		return nil, fmt.Errorf("reserve helper result file: %w", err)
	}
	resultPath := result.Name()
	if err := result.Close(); err != nil {
		_ = os.Remove(resultPath)
		_ = os.Remove(backupPath)
		_ = os.Remove(staged)
		return nil, err
	}
	if err := os.Remove(resultPath); err != nil {
		_ = os.Remove(backupPath)
		_ = os.Remove(staged)
		return nil, fmt.Errorf("prepare helper result path: %w", err)
	}
	if err := i.supervisor.schedule(ctx, applyRequest{
		target: i.target,
		staged: staged,
		backup: backupPath,
		result: resultPath,
		oldPID: os.Getpid(),
	}); err != nil {
		_ = os.Remove(resultPath)
		_ = os.Remove(backupPath)
		_ = os.Remove(staged)
		return nil, err
	}
	if i.logger != nil {
		i.logger.Printf("update: supervisor will retain the previous executable at %s for recovery", backupPath)
	}
	return waitForApplyResult(resultPath, helperResultTimeout), nil
}

func validateApplyPaths(req applyRequest) error {
	if !filepath.IsAbs(req.target) || !filepath.IsAbs(req.staged) || !filepath.IsAbs(req.backup) || !filepath.IsAbs(req.result) {
		return errors.New("update paths must be absolute")
	}
	dir := filepath.Dir(req.target)
	if filepath.Dir(req.staged) != dir || filepath.Dir(req.backup) != dir || filepath.Dir(req.result) != dir {
		return errors.New("update paths must share the executable directory")
	}
	if req.target == req.staged || req.target == req.backup || req.target == req.result ||
		req.staged == req.backup || req.staged == req.result || req.backup == req.result {
		return errors.New("update paths must be distinct")
	}
	if !strings.HasPrefix(filepath.Base(req.staged), ".plans-update-") ||
		!strings.HasPrefix(filepath.Base(req.backup), ".plans-backup-") ||
		!strings.HasPrefix(filepath.Base(req.result), ".plans-update-result-") {
		return errors.New("invalid update staging paths")
	}
	if req.oldPID <= 0 {
		return errors.New("invalid original process ID")
	}
	return nil
}

func applyArgs(req applyRequest) []string {
	return []string{
		"__apply-update",
		"--target", req.target,
		"--staged", req.staged,
		"--backup", req.backup,
		"--result", req.result,
		"--old-pid", strconv.Itoa(req.oldPID),
	}
}

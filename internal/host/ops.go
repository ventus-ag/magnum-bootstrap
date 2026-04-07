package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ventus-ag/magnum-bootstrap/internal/logging"
)

type Change struct {
	Action  string `json:"action"`
	Path    string `json:"path,omitempty"`
	Summary string `json:"summary"`
}

const (
	ActionCreate  = "create"
	ActionUpdate  = "update"
	ActionDelete  = "delete"
	ActionReplace = "replace"
	ActionReload  = "reload"
	ActionRestart = "restart"
	ActionRead    = "read"
	ActionOther   = "other"
)

type Executor struct {
	Apply      bool
	Logger     *logging.Logger
	HTTPClient *http.Client
}

type DownloadResult struct {
	Checksum string
	Changed  bool
	Change   *Change
}

func NewExecutor(apply bool, logger *logging.Logger) *Executor {
	return &Executor{Apply: apply, Logger: logger}
}

func (e *Executor) EnsureDir(path string, mode os.FileMode) (*Change, error) {
	info, err := os.Stat(path)
	switch {
	case err == nil && !info.IsDir():
		return nil, fmt.Errorf("%s exists and is not a directory", path)
	case err == nil:
		if info.Mode().Perm() == mode.Perm() {
			return nil, nil
		}
		change := &Change{Action: ActionUpdate, Path: path, Summary: fmt.Sprintf("set directory mode %s to %04o", path, mode.Perm())}
		return change, e.applyFunc(change, func() error { return os.Chmod(path, mode) })
	case !os.IsNotExist(err):
		return nil, err
	default:
		change := &Change{Action: ActionCreate, Path: path, Summary: fmt.Sprintf("create directory %s", path)}
		return change, e.applyFunc(change, func() error { return os.MkdirAll(path, mode) })
	}
}

func (e *Executor) EnsureFile(path string, content []byte, mode os.FileMode) (*Change, error) {
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, statErr
		}
		if bytes.Equal(current, content) && info.Mode().Perm() == mode.Perm() {
			return nil, nil
		}
		action := ActionUpdate
		summary := fmt.Sprintf("update file %s", path)
		if !bytes.Equal(current, content) {
			action = ActionReplace
			summary = fmt.Sprintf("replace file %s", path)
		}
		change := &Change{Action: action, Path: path, Summary: summary}
		return change, e.applyFunc(change, func() error { return writeFileAtomic(path, content, mode) })
	case os.IsNotExist(err):
		change := &Change{Action: ActionCreate, Path: path, Summary: fmt.Sprintf("create file %s", path)}
		return change, e.applyFunc(change, func() error { return writeFileAtomic(path, content, mode) })
	default:
		return nil, err
	}
}

func (e *Executor) EnsureCopy(src, dst string, mode os.FileMode) (*Change, error) {
	content, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	change, err := e.EnsureFile(dst, content, mode)
	if err != nil || change == nil {
		return change, err
	}
	switch change.Action {
	case ActionCreate:
		change.Summary = fmt.Sprintf("copy %s to %s", src, dst)
	case ActionReplace:
		change.Summary = fmt.Sprintf("replace %s from %s", dst, src)
	default:
		change.Summary = fmt.Sprintf("update %s from %s", dst, src)
	}
	return change, nil
}

func (e *Executor) DownloadFileWithRetry(ctx context.Context, url, path string, mode os.FileMode, retries int) (DownloadResult, error) {
	if retries < 1 {
		retries = 1
	}

	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		result, err := e.downloadFileOnce(ctx, url, path, mode)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt == retries {
			break
		}
		wait := time.Duration(attempt) * time.Second
		if e.Logger != nil {
			e.Logger.Warnf("download attempt %d/%d failed for %s: %v; retrying in %s", attempt, retries, url, err, wait)
		}
		select {
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}

	return DownloadResult{}, fmt.Errorf("download %s after %d attempt(s): %w", url, retries, lastErr)
}

func (e *Executor) EnsureAbsent(path string) (*Change, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	change := &Change{Action: ActionDelete, Path: path, Summary: fmt.Sprintf("remove %s", path)}
	return change, e.applyFunc(change, func() error { return os.Remove(path) })
}

func (e *Executor) EnsureLine(path, line string, mode os.FileMode) (*Change, error) {
	current, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		content := []byte(line + "\n")
		change := &Change{Action: ActionCreate, Path: path, Summary: fmt.Sprintf("create %s with required line", path)}
		return change, e.applyFunc(change, func() error { return writeFileAtomic(path, content, mode) })
	case err != nil:
		return nil, err
	}

	lines := strings.Split(strings.TrimRight(string(current), "\n"), "\n")
	for _, existing := range lines {
		if existing == line {
			return nil, nil
		}
	}
	if len(current) > 0 && current[len(current)-1] != '\n' {
		current = append(current, '\n')
	}
	current = append(current, []byte(line+"\n")...)
	change := &Change{Action: ActionUpdate, Path: path, Summary: fmt.Sprintf("append required line to %s", path)}
	return change, e.applyFunc(change, func() error { return writeFileAtomic(path, current, mode) })
}

func (e *Executor) UpsertExport(path, varName, value string, mode os.FileMode) (*Change, error) {
	current, err := os.ReadFile(path)
	fileMissing := false
	switch {
	case os.IsNotExist(err):
		current = nil
		fileMissing = true
	case err != nil:
		return nil, err
	}

	lines := filterExportLines(strings.Split(normalizeLines(string(current)), "\n"), varName)
	if value != "" {
		lines = append(lines, fmt.Sprintf("export %s='%s'", varName, quoteShellValue(value)))
	}

	content := strings.TrimRight(strings.Join(dropEmptyTrailingLines(lines), "\n"), "\n")
	if content != "" {
		content += "\n"
	}

	currentContent := normalizeEmpty(string(current))
	if currentContent == content {
		return nil, nil
	}

	if content == "" {
		change := &Change{Action: ActionDelete, Path: path, Summary: fmt.Sprintf("remove export %s from %s", varName, path)}
		return change, e.applyFunc(change, func() error { return writeFileAtomic(path, []byte(content), mode) })
	}

	action := ActionUpdate
	if fileMissing {
		action = ActionCreate
	}
	change := &Change{Action: action, Path: path, Summary: fmt.Sprintf("upsert export %s in %s", varName, path)}
	return change, e.applyFunc(change, func() error { return writeFileAtomic(path, []byte(content), mode) })
}

func (e *Executor) Run(name string, args ...string) error {
	if !e.Apply {
		return nil
	}

	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	if e.Logger != nil && len(output) > 0 {
		e.Logger.Infof("%s %s output=%s", name, strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// RunCapture executes a command and returns its stdout. Unlike Run it always
// executes regardless of the Apply flag because callers need the output to
// make decisions (e.g. checking etcd membership, getting instance IDs).
func (e *Executor) RunCapture(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if e.Logger != nil && stderr.Len() > 0 {
		e.Logger.Infof("%s %s stderr=%s", name, strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", fmt.Errorf("%s %s: %w (stderr: %s)", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Systemctl runs a systemctl command. Skipped in dry-run mode for mutating
// actions (daemon-reload, enable, disable, start, stop, restart) but always
// runs for read-only actions (is-active, is-enabled, status).
func (e *Executor) Systemctl(action string, units ...string) error {
	args := append([]string{action}, units...)
	switch action {
	case "is-active", "is-enabled", "status":
		_, err := e.RunCapture("systemctl", args...)
		return err
	default:
		return e.Run("systemctl", args...)
	}
}

// SystemctlIsActive returns true if the given unit is active.
func (e *Executor) SystemctlIsActive(unit string) bool {
	out, err := e.RunCapture("systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

// SystemctlExists returns true if the given unit file is known to systemd.
func (e *Executor) SystemctlExists(unit string) bool {
	_, err := e.RunCapture("systemctl", "cat", unit)
	return err == nil
}

// SystemctlIsEnabled returns true if the given unit is enabled.
func (e *Executor) SystemctlIsEnabled(unit string) bool {
	out, err := e.RunCapture("systemctl", "is-enabled", unit)
	return err == nil && strings.TrimSpace(out) == "enabled"
}

// WaitForSystemctlActive polls until the given unit becomes active or the
// timeout expires.
func (e *Executor) WaitForSystemctlActive(unit string, timeout, interval time.Duration) bool {
	stableFor := 3 * interval
	if stableFor < 3*time.Second {
		stableFor = 3 * time.Second
	}
	if stableFor > 10*time.Second {
		stableFor = 10 * time.Second
	}
	return waitForActiveState(func() bool {
		return e.SystemctlIsActive(unit)
	}, timeout, interval, stableFor)
}

func waitForActiveState(check func() bool, timeout, interval, stableFor time.Duration) bool {
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(timeout)
	var activeSince time.Time
	for {
		if check() {
			if activeSince.IsZero() {
				activeSince = time.Now()
			}
			if time.Since(activeSince) >= stableFor {
				return true
			}
		} else {
			activeSince = time.Time{}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// IsMountpoint returns true if the given path is a mounted filesystem.
func (e *Executor) IsMountpoint(path string) bool {
	_, err := e.RunCapture("mountpoint", "-q", path)
	return err == nil
}

func (e *Executor) applyFunc(change *Change, fn func() error) error {
	if e.Logger != nil {
		e.Logger.Infof("action=%s path=%s %s", change.Action, change.Path, change.Summary)
	}
	if !e.Apply {
		return nil
	}
	return fn()
}

func writeFileAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func FileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func CopyFileAtomic(src, dst string, mode os.FileMode) error {
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, content, mode)
}

func filterExportLines(lines []string, varName string) []string {
	filtered := make([]string, 0, len(lines))
	prefixes := []string{
		"export " + varName + "=",
		"declare -x " + varName + "=",
	}
	for _, line := range lines {
		if line == "" {
			filtered = append(filtered, line)
			continue
		}
		keep := true
		for _, prefix := range prefixes {
			if strings.HasPrefix(line, prefix) {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

func quoteShellValue(value string) string {
	return strings.ReplaceAll(value, "'", "'\\''")
}

func normalizeLines(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.TrimRight(content, "\n")
}

func normalizeEmpty(content string) string {
	if content == "" {
		return ""
	}
	return normalizeLines(content) + "\n"
}

func dropEmptyTrailingLines(lines []string) []string {
	last := len(lines)
	for last > 0 && lines[last-1] == "" {
		last--
	}
	return lines[:last]
}

func (e *Executor) downloadFileOnce(ctx context.Context, url, path string, mode os.FileMode) (DownloadResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DownloadResult{}, err
	}

	resp, err := e.httpClient().Do(req)
	if err != nil {
		return DownloadResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DownloadResult{}, fmt.Errorf("unexpected status %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return DownloadResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return DownloadResult{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		tmp.Close()
		return DownloadResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return DownloadResult{}, err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return DownloadResult{}, err
	}

	newChecksum := hex.EncodeToString(hasher.Sum(nil))

	currentChecksum, currentErr := FileSHA256(path)
	action := ActionCreate
	summary := fmt.Sprintf("download file %s from %s", path, url)
	if currentErr == nil {
		info, err := os.Stat(path)
		if err != nil {
			return DownloadResult{}, err
		}
		if currentChecksum == newChecksum && info.Mode().Perm() == mode.Perm() {
			return DownloadResult{Checksum: newChecksum, Changed: false}, nil
		}
		action = ActionReplace
		summary = fmt.Sprintf("replace file %s from %s", path, url)
		if currentChecksum == newChecksum && info.Mode().Perm() != mode.Perm() {
			action = ActionUpdate
			summary = fmt.Sprintf("update file mode %s after download from %s", path, url)
		}
	} else if !os.IsNotExist(currentErr) {
		return DownloadResult{}, currentErr
	}

	change := &Change{
		Action:  action,
		Path:    path,
		Summary: summary,
	}
	if err := e.applyFunc(change, func() error { return os.Rename(tmpPath, path) }); err != nil {
		return DownloadResult{}, err
	}

	return DownloadResult{Checksum: newChecksum, Changed: true, Change: change}, nil
}

func (e *Executor) httpClient() *http.Client {
	if e != nil && e.HTTPClient != nil {
		return e.HTTPClient
	}
	return http.DefaultClient
}

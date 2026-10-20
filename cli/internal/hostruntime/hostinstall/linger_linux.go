//go:build linux

package hostinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const (
	lingerBaselineSchema = "paperboat.linger-baseline/v1"
	lingerBaselineName   = "linger-baseline.json"
	lingerCommandTimeout = 20 * time.Second
)

// lingerDirectory and runLoginctl are variables so tests can use a temporary directory and a fake
// loginctl; production always uses systemd-logind's own state and command.
var (
	lingerDirectory = "/var/lib/systemd/linger"
	systemdRuntime  = "/run/systemd/system"
	runLoginctl     = func(ctx context.Context, arguments ...string) error {
		ctx, cancel := context.WithTimeout(ctx, lingerCommandTimeout)
		defer cancel()
		for _, candidate := range []string{"/usr/bin/loginctl", "/bin/loginctl"} {
			if _, err := os.Stat(candidate); err != nil {
				continue
			}
			if output, err := exec.CommandContext(ctx, candidate, arguments...).CombinedOutput(); err != nil {
				return fmt.Errorf("loginctl %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
			}
			return nil
		}
		return errors.New("loginctl is not installed")
	}
	lingerUserName = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	// stateOwner is the account that must own the baseline: root in production.
	stateOwner = func() int { return 0 }
)

// lingerBaseline records that Paperboat, not the administrator, turned lingering on for a user.
// It exists only while that is true, so uninstall restores exactly the setting it found.
type lingerBaseline struct {
	Schema string `json:"schema"`
	User   string `json:"user"`
}

func lingerApplies(request Request) bool {
	if _, err := os.Stat(systemdRuntime); err != nil {
		return false
	}
	return request.UID > 0
}

func userLingers(user string) (bool, error) {
	if !lingerUserName.MatchString(user) {
		return false, ErrInvalidRequest
	}
	_, err := os.Lstat(filepath.Join(lingerDirectory, user))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ensureUserLinger keeps the user's systemd instance, and so the Paperboat daemon, running after
// every login session ends. Without it a remote machine goes offline as soon as its last ssh
// session closes. A setting the administrator already made is left alone.
func ensureUserLinger(ctx context.Context, request Request, paths installPaths) error {
	if !lingerApplies(request) {
		return nil
	}
	enabled, err := userLingers(request.User)
	if err != nil {
		return hostInstallFailure("read whether the user stays signed in after logout", err)
	}
	if enabled {
		return nil
	}
	baselinePath := filepath.Join(paths.runtimeState, lingerBaselineName)
	if err := writeLingerBaseline(baselinePath, request.User); err != nil {
		return hostInstallFailure("record the logout setting before changing it", err)
	}
	if err := runLoginctl(ctx, "enable-linger", request.User); err != nil {
		return errors.Join(hostInstallFailure("keep Paperboat running after you log out (loginctl enable-linger)", err), os.Remove(baselinePath))
	}
	return nil
}

// restoreUserLinger undoes ensureUserLinger when Paperboat is the one that enabled it.
func restoreUserLinger(ctx context.Context, request Request, paths installPaths) error {
	baselinePath := filepath.Join(paths.runtimeState, lingerBaselineName)
	baseline, err := readLingerBaseline(baselinePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return hostInstallFailure("read the recorded logout setting", err)
	}
	enabled, err := userLingers(baseline.User)
	if err != nil {
		return hostInstallFailure("read whether the user stays signed in after logout", err)
	}
	if enabled {
		if err := runLoginctl(ctx, "disable-linger", baseline.User); err != nil {
			return hostInstallFailure("restore the logout setting (loginctl disable-linger)", err)
		}
	}
	if err := os.Remove(baselinePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeLingerBaseline(path, user string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Dir(path)); err != nil || !info.IsDir() || ownerUID(info) != stateOwner() {
		return errors.Join(ErrInvalidRequest, err)
	}
	body, err := json.Marshal(lingerBaseline{Schema: lingerBaselineSchema, User: user})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.Options{Mode: 0o600, OwnerUID: stateOwner(), OwnerGID: -1})
}

func readLingerBaseline(path string) (lingerBaseline, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return lingerBaseline{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() < 1 || info.Size() > 4<<10 || ownerUID(info) != stateOwner() {
		return lingerBaseline{}, ErrInvalidRequest
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return lingerBaseline{}, err
	}
	var baseline lingerBaseline
	if err := decodeInstallJSON(strings.NewReader(string(body)), &baseline, 4<<10); err != nil {
		return lingerBaseline{}, err
	}
	if baseline.Schema != lingerBaselineSchema || !lingerUserName.MatchString(baseline.User) {
		return lingerBaseline{}, ErrInvalidRequest
	}
	return baseline, nil
}

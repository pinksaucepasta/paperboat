//go:build darwin || linux

package hostinstall

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func workerEnvironment(request Request) map[string]string {
	controlHost := ""
	if parsed, err := url.Parse(request.ControlURL); err == nil {
		controlHost = parsed.Hostname()
	}
	releaseHost := ""
	if parsed, err := url.Parse(request.Artifact.RepositoryURL); err == nil {
		releaseHost = parsed.Hostname()
	}
	directHosts := controlHost
	if releaseHost != "" && releaseHost != controlHost {
		directHosts += "," + releaseHost
	}
	return map[string]string{
		"HOME": request.Home, "PATH": request.Path,
		"PAPERBOAT_RUNTIME_STATE_ROOT": request.StateRoot, "PAPERBOAT_WORKSPACE_ROOT": request.WorkspaceRoot,
		"PAPERBOAT_SESSION_OWNER_CATALOG": filepath.Join(platformPaths(request.UID).root, "session-owners"),
		"PAPERBOAT_CONTROL_URL":           request.ControlURL, "PAPERBOAT_MACHINE_ID": request.UserMachineID,
		"PAPERBOAT_SHELL": request.Shell, "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": request.HelperListenAddress,
		"PAPERBOAT_RUNTIME_SERVICE_SCOPE": "system",
		// Paperboat's own narrow private-preview PAC applies to browsers. The
		// managed control plane must remain direct so enabling that PAC cannot
		// prevent hostd/updater from starting.
		"PAPERBOAT_NO_PROXY": directHosts,
	}
}

func invokingUID() int {
	value := os.Getenv("SUDO_UID")
	if value == "" {
		value = os.Getenv("PAPERBOAT_INVOKING_UID")
	}
	uid, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return uid
}

func verifyArtifact(path string, uid int, platform string) (resultErr error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	lstat, err := os.Lstat(path)
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if problem := suppliedExecutableProblem(path, lstat, uid); problem != nil {
		return problem
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { resultErr = withCleanup("installation cleanup failed", resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(lstat, info) || info.Mode().Perm()&0o022 != 0 || info.Size() < 1 || info.Size() > 256<<20 || (ownerUID(info) != uid && ownerUID(info) != 0) {
		return ErrInvalidRequest
	}
	return nil
}

func suppliedExecutableProblem(path string, info os.FileInfo, uid int) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
		return &SuppliedExecutableError{Path: path, Reason: "is not a regular file", Fix: "run the real pb file directly, not a link or directory"}
	case info.Mode().Perm()&0o022 != 0:
		return &SuppliedExecutableError{Path: path, Reason: fmt.Sprintf("is writable by other users (mode %04o)", info.Mode().Perm()), Fix: "run `chmod go-w " + path + "`"}
	case ownerUID(info) != uid && ownerUID(info) != 0:
		return &SuppliedExecutableError{Path: path, Reason: "is owned by another user", Fix: "copy it to a file you own (or run `sudo chown " + strconv.Itoa(uid) + " " + path + "`)"}
	}
	return nil
}

// CheckSuppliedExecutable runs the privileged installer's file checks as the
// invoking user, before any administrator prompt.
func CheckSuppliedExecutable(path string, uid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil {
		return hostInstallFailure("inspecting installation state failed", ErrInvalidRequest, err)
	}
	return suppliedExecutableProblem(path, info, uid)
}

func canonicalOwnedDirectory(path string, uid int) bool {
	return checkCanonicalOwnedDirectory(path, uid) == nil
}

func checkCanonicalOwnedDirectory(path string, uid int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if resolved != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || ownerUID(info) != uid {
		return ErrInvalidRequest
	}
	return nil
}

func canonicalExecutable(path string) bool { return checkCanonicalExecutable(path) == nil }

func checkCanonicalExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidRequest
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return ErrInvalidRequest
	}
	return nil
}

func pathListValid(value string) bool {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, path := range filepath.SplitList(value) {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return false
		}
	}
	return true
}

func ownerUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

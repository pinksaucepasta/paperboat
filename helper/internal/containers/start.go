package containers

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func bindMount(path string, readOnly bool) string {
	fields := []string{"type=bind", "src=" + path, "dst=" + path}
	if readOnly {
		fields = append(fields, "readonly")
	}
	var out bytes.Buffer
	writer := csv.NewWriter(&out)
	writer.Write(fields)
	writer.Flush()
	return strings.TrimSuffix(out.String(), "\n")
}

// containerName uses the service name; capability remains the control-plane contract.
func containerName(owner, capability string) string {
	if capability == "tunnel" {
		capability = "edge"
	}
	return "paperboat-pbh-" + owner + "-" + capability
}

func (l *Launcher) startContainer(ctx context.Context, capability, image string) error {
	return l.startContainerNamed(ctx, capability, image, containerName(installationOwner(l.StateDir), capability))
}

// SentryDSNFile is the optional owner-only file in the state directory that holds the Sentry DSN for the relay and edge
// containers. The containers get its path (the state directory is mounted at the same path) and the enabling switch,
// never the DSN itself: a value in `docker run -e` would show in the process list and `docker inspect`. The release
// is pbh's own version: the reporting library refuses a DSN without one. Without the file the containers report nothing.
const SentryDSNFile = "sentry-dsn"

const maxSentryDSNBytes = 4096

var sentryReleasePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// sentryEnvironment returns the docker flags that turn reporting on. A DSN file or release that cannot be used only
// turns reporting off: the returned error says why and the caller warns; it must never keep a relay or edge from starting.
func sentryEnvironment(stateDir, release string) ([]string, error) {
	path := filepath.Join(stateDir, SentryDSNFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSentryDSNBytes || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Sentry DSN file " + path + " must be a regular file of at most 4 KiB with owner-only permissions (mode 0600); fix or remove it")
	}
	if !sentryReleasePattern.MatchString(release) {
		return nil, errors.New("pbh's own version is not a usable Sentry release")
	}
	return []string{"--env", "PAPERBOAT_SENTRY_ENABLED=true", "--env", "PAPERBOAT_SENTRY_DSN_FILE=" + path, "--env", "PAPERBOAT_SENTRY_RELEASE=" + release}, nil
}

func (l *Launcher) startContainerNamed(ctx context.Context, capability, image, name string) error {
	owner := installationOwner(l.StateDir)
	args := []string{"run", "-d", "--name", name, "--label", ownerLabel + "=" + owner, "--network", "host", "--user", "0:0", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m", "--mount", bindMount(l.StateDir, false)}
	sentry, warning := sentryEnvironment(l.StateDir, l.Release)
	if warning != nil {
		fmt.Fprintln(os.Stderr, "pbh: error reporting is off for the", capability, "container:", warning)
	}
	args = append(args, sentry...)
	seen := map[string]bool{}
	for _, path := range l.ReadOnlyFiles {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		if !filepath.IsAbs(path) {
			return errors.New("runtime trust file must have an absolute path")
		}
		relative, err := filepath.Rel(l.StateDir, path)
		if err != nil {
			return err
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return errors.New("runtime trust file must be a bounded regular file")
		}
		args = append(args, "--mount", bindMount(path, true))
	}
	args = append(args, image, "run", "--state-dir", filepath.Join(l.StateDir, capability))
	if _, err := docker(ctx, args...); err != nil {
		return errors.New("start runtime container failed; installation state retained")
	}
	return nil
}

// RemoveRenamedRuntime removes the former capability-named container after the
// new service is ready and the previous supervisor has stopped.
func RemoveRenamedRuntime(ctx context.Context, dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("installation directory must be absolute")
	}
	owner := installationOwner(dir)
	return removeOwned(ctx, "paperboat-pbh-"+owner+"-tunnel", owner)
}

package containers

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
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

func (l *Launcher) startContainerNamed(ctx context.Context, capability, image, name string) error {
	owner := installationOwner(l.StateDir)
	args := []string{"run", "-d", "--name", name, "--label", ownerLabel + "=" + owner, "--network", "host", "--user", "0:0", "--cap-drop", "ALL", "--cap-add", "NET_BIND_SERVICE", "--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=16m", "--mount", bindMount(l.StateDir, false)}
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

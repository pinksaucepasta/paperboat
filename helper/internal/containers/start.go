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

func (l *Launcher) startContainer(ctx context.Context, capability, image string) error {
	owner := installationOwner(l.StateDir)
	name := "paperboat-pbh-" + owner + "-" + capability
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

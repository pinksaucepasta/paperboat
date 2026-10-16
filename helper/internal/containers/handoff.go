package containers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const handoffFile = "runtime-handoff.json"

func resolvedImage(ctx context.Context, image string) (string, error) {
	b, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return "", errors.New("resolve runtime image identity failed")
	}
	id := strings.TrimSpace(string(b))
	if !strings.HasPrefix(id, "sha256:") || len(id) != 71 {
		return "", errors.New("runtime image identity is invalid")
	}
	return id, nil
}

// PrepareHandoff transfers unchanged containers to the next supervisor. A
// changed image still follows the ordinary replacement and readiness path.
// The returned roles keep their process epoch, including admitted connections.
func PrepareHandoff(ctx context.Context, dir string, images Images) (map[string]bool, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("installation directory must be absolute")
	}
	preserve := map[string]string{}
	roles := map[string]bool{}
	owner := installationOwner(dir)
	for _, role := range []string{"relay", "tunnel"} {
		image, err := images.Image(role)
		if err != nil {
			continue
		}
		id, err := resolvedImage(ctx, image)
		if err != nil {
			return nil, err
		}
		info, exists, err := inspectContainer(ctx, "paperboat-pbh-"+owner+"-"+role)
		if err != nil {
			return nil, err
		}
		if exists && info.Owner != owner {
			return nil, errors.New("runtime container belongs to another installation; handoff refused")
		}
		if exists && info.Running && info.ImageID == id {
			preserve[role] = id
			roles[role] = true
		}
	}
	b, err := json.Marshal(preserve)
	if err != nil {
		return nil, err
	}
	temporary, err := os.CreateTemp(dir, ".runtime-handoff-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(temporary.Name())
	if err = temporary.Chmod(0600); err == nil {
		_, err = temporary.Write(b)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(temporary.Name(), filepath.Join(dir, handoffFile)); err != nil {
		return nil, err
	}
	return roles, nil
}

func preservesHandoff(dir, role, imageID string) bool {
	path := filepath.Join(dir, handoffFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(b) > 16384 {
		return false
	}
	var preserved map[string]string
	return json.Unmarshal(b, &preserved) == nil && imageID != "" && preserved[role] == imageID
}

func ClearHandoff(dir string) error {
	if err := os.Remove(filepath.Join(dir, handoffFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

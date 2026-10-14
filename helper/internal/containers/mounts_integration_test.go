package containers

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDockerRuntimeReadsExternalTrustFilesWithoutWriteAccess(t *testing.T) {
	base := os.Getenv("PAPERBOAT_TEST_DOCKER_BASE_IMAGE")
	if base == "" {
		t.Skip("explicit local Docker base image required")
	}
	if _, err := (Images{Relay: base}).Image("relay"); err != nil {
		t.Fatal("invalid fixture image reference")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "certificate,key.pem")
	payload := []byte("trust-file-fixture\n")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(root, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM "+base+"\nENTRYPOINT [\"sh\",\"-c\",\"sleep 60\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner := installationOwner(dir)
	image := "paperboat-mount-fixture:" + owner
	name := "paperboat-pbh-" + owner + "-relay"
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := removeOwned(cleanup, name, owner); err != nil {
			t.Error("owned fixture container cleanup failed")
		}
		docker(cleanup, "image", "rm", image)
	})
	if _, err := docker(ctx, "build", "--tag", image, "--file", dockerfile, root); err != nil {
		t.Fatal("build filesystem fixture failed")
	}
	launcher := Launcher{StateDir: dir, ReadOnlyFiles: []string{path}}
	if err := launcher.startContainer(ctx, "relay", image); err != nil {
		t.Fatal(err)
	}
	content, err := docker(ctx, "exec", name, "cat", path)
	if err != nil || !bytes.Equal(content, payload) {
		t.Fatal("external trust file was not readable at its original absolute path")
	}
	if _, err := docker(ctx, "exec", name, "sh", "-c", `printf changed > "$1"`, "write-probe", path); err == nil {
		t.Fatal("runtime could overwrite externally managed trust file")
	}
	content, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(content, payload) {
		t.Fatal("external trust file changed")
	}
}

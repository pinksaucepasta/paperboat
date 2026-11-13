package containers

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// replacementImage commits the fixture image under a new image id whose entrypoint runs script,
// so the launcher's "run --state-dir" arguments are ignored.
func replacementImage(t *testing.T, ctx context.Context, base, script string) string {
	t.Helper()
	tag := "paperboat-handover-test:" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	scratch := "paperboat-handover-commit-" + installationOwner(t.TempDir())
	if _, err := docker(ctx, "create", "--name", scratch, base, "true"); err != nil {
		t.Fatal("create commit source failed")
	}
	defer func() { _, _ = docker(context.Background(), "rm", scratch) }()
	if _, err := docker(ctx, "commit", "--change", `ENTRYPOINT ["/bin/sh","-c","`+script+`","--"]`, scratch, tag); err != nil {
		t.Fatal("commit replacement image failed")
	}
	t.Cleanup(func() { _, _ = docker(context.Background(), "rmi", "-f", tag) })
	return tag
}

// An install of a new tunnel image hands over instead of stopping: the serving container is
// never stopped by the installer (it exits on its own once it has drained), and only then does
// the replacement take its name. A replacement that dies first is removed and the serving
// container is untouched.
func TestDockerEdgeInstallHandsOverWithoutStoppingTheServingContainer(t *testing.T) {
	base := os.Getenv("PAPERBOAT_TEST_DOCKER_BASE_IMAGE")
	if base == "" {
		t.Skip("explicit local Docker fixture image required")
	}
	for _, mode := range []string{"takes over", "dies first"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "tunnel"), 0o700); err != nil {
				t.Fatal(err)
			}
			owner := installationOwner(dir)
			name, next := "paperboat-pbh-"+owner+"-edge", "paperboat-pbh-"+owner+"-edge-next"
			serving := "sleep 120"
			if mode == "takes over" {
				serving = "sleep 4" // the serving process exits on its own once drained
			}
			if _, err := docker(ctx, "run", "-d", "--name", name, "--label", ownerLabel+"="+owner, "--entrypoint", "/bin/sh", base, "-c", serving); err != nil {
				t.Fatal("start serving fixture failed")
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
				defer stop()
				_ = removeOwned(cleanup, next, owner)
				_ = removeOwned(cleanup, name, owner)
			})
			servingID, err := docker(ctx, "inspect", "--format", "{{.Id}}", name)
			if err != nil {
				t.Fatal(err)
			}
			script := "sleep 120"
			if mode == "dies first" {
				script = "echo replacement failed on purpose; exit 3"
			}
			image := replacementImage(t, ctx, base, script)
			launcher := Launcher{StateDir: dir, Images: Images{Edge: image}}
			var journal bytes.Buffer
			err = launcher.handOver(ctx, "tunnel", image, name, owner, &journal)
			if mode == "dies first" {
				var exit *ChildExit
				if !errors.As(err, &exit) {
					t.Fatalf("handover error = %v, want the replacement's exit", err)
				}
				if after, err := docker(ctx, "inspect", "--format", "{{.Id}}", name); err != nil || string(after) != string(servingID) {
					t.Fatal("a failed replacement disturbed the serving container")
				}
				if running, err := containerRunning(ctx, name, owner); err != nil || !running {
					t.Fatal("a failed replacement stopped the serving container")
				}
				if _, exists, _ := inspectContainer(ctx, next); exists {
					t.Fatal("the failed replacement was left behind")
				}
				if journal.Len() == 0 {
					t.Fatal("the failed replacement's output was removed without reaching the journal")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, exists, err := inspectContainer(ctx, name)
			if err != nil || !exists || !info.Running {
				t.Fatal("the replacement does not serve under the role's name")
			}
			want, err := resolvedImage(ctx, image)
			if err != nil || info.ImageID != want {
				t.Fatal("the role's name does not run the installed image")
			}
			if _, exists, _ := inspectContainer(ctx, next); exists {
				t.Fatal("the replacement kept its temporary name")
			}
		})
	}
}

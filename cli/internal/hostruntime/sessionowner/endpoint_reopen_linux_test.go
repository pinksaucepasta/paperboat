//go:build linux

package sessionowner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
)

func TestCatalogInstallReopensRetiredEndpointForEnrolledUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root catalog and an unprivileged native child")
	}
	root, err := os.MkdirTemp("/var/lib", "pb-endpoint-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err = os.WriteFile(pin, image, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "endpoint-fixture", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "runtime")
	if err = os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chown(state, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Schema: Schema, Generation: generation(1), Instance: "endpoint-fixture", Source: source, Executable: pin,
		RuntimeRoot: state, Endpoint: filepath.Join(root, "retired-run", "control.sock"), UID: 65534, GID: 65534, Workspace: root, Shell: "/bin/sh"}
	runner := &endpointNativeRunner{descriptor: d}
	catalog := Catalog{Root: filepath.Join(root, "catalog"), ConfigRoot: root, User: "nobody", Group: "nogroup", Maximum: 4, Runner: runner}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	runner.ctx = ctx
	defer runner.stop()
	if err = catalog.Publish(ctx, d); err != nil {
		t.Fatal(err)
	}
	// Retirement removes the socket directory, but retains its protected descriptor.
	if err = catalog.Install(ctx, d); err != nil {
		t.Fatalf("reopen archived native owner: %v", err)
	}
	info, err := os.Stat(filepath.Dir(d.Endpoint))
	if err != nil {
		t.Fatal(err)
	}
	owner := info.Sys().(*syscall.Stat_t)
	if owner.Uid != 65534 || owner.Gid != 65534 || info.Mode().Perm() != 0700 {
		t.Fatal("endpoint was not private to enrolled account")
	}
	if info, err = os.Stat(d.Endpoint); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatal("native child could not bind the reopened endpoint")
	}
	runner.stop()
	if err = os.Remove(filepath.Dir(d.Endpoint)); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"foreign_owner", "shared_mode", "symlink"} {
		t.Run(kind, func(t *testing.T) { assertUnsafeEndpointPreserved(t, ctx, catalog, d, runner, kind) })
	}
}

func assertUnsafeEndpointPreserved(t *testing.T, ctx context.Context, catalog Catalog, d Descriptor, runner *endpointNativeRunner, kind string) {
	t.Helper()
	directory := filepath.Dir(d.Endpoint)
	target := directory
	if kind == "symlink" {
		target = directory + "-target"
	}
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(target)
	if kind == "foreign_owner" {
		if err := os.Chmod(target, 0700); err != nil {
			t.Fatal(err)
		}
	} else if kind == "shared_mode" {
		if err := os.Chown(target, d.UID, d.GID); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.Symlink(target, directory); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(directory)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Install(ctx, d); err == nil || runner.running {
		t.Fatal("unsafe endpoint started an owner")
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid {
		t.Fatal("refusal modified an unsafe directory or symlink target")
	}
}

// The native-controller boundary launches a real child under the enrolled UID;
// no fake listener or successful service receipt can conceal a missing directory.
type endpointNativeRunner struct {
	ctx        context.Context
	descriptor Descriptor
	child      *exec.Cmd
	input      io.WriteCloser
	running    bool
}

func (r *endpointNativeRunner) Run(ctx context.Context, name string, args ...string) error {
	if name != "systemctl" || len(args) == 0 {
		return errors.New("unexpected native boundary")
	}
	switch args[0] {
	case "start":
		r.child = exec.CommandContext(r.ctx, r.descriptor.Executable, "-test.run=^TestEndpointNativeChild$")
		r.child.Env = append(os.Environ(), "PB_ENDPOINT_CHILD="+r.descriptor.Endpoint)
		r.child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(r.descriptor.UID), Gid: uint32(r.descriptor.GID)}}
		var err error
		r.input, err = r.child.StdinPipe()
		if err != nil {
			return err
		}
		output, err := r.child.StdoutPipe()
		if err != nil {
			return err
		}
		if err = r.child.Start(); err != nil {
			return err
		}
		ready, err := bufio.NewReader(output).ReadString('\n')
		if err != nil || ready != "READY\n" {
			return fmt.Errorf("unprivileged native listener failed: %s: %v", ready, err)
		}
		r.running = true
		return nil
	case "enable":
		if len(args) > 1 && args[1] == "--now" {
			return r.Run(ctx, name, "start")
		}
		return nil
	case "daemon-reload", "is-active", "disable", "reset-failed":
		return nil
	case "stop":
		r.stop()
		return nil
	default:
		return errors.New("unexpected native operation")
	}
}
func (r *endpointNativeRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	if name != "systemctl" || len(args) == 0 || args[0] != "show" {
		return "", errors.New("unexpected native inspection")
	}
	if r.running {
		return "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\n", nil
	}
	return "LoadState=not-found\n", nil
}
func (r *endpointNativeRunner) stop() {
	r.running = false
	if r.input != nil {
		_ = r.input.Close()
		r.input = nil
	}
	if r.child != nil {
		_ = r.child.Wait()
		r.child = nil
	}
}
func TestEndpointNativeChild(t *testing.T) {
	endpoint := os.Getenv("PB_ENDPOINT_CHILD")
	if endpoint == "" {
		return
	}
	if os.Geteuid() != 65534 {
		t.Fatal("native child did not enter the enrolled account")
	}
	listener, err := hostdproto.ListenWorkloads(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("READY")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

//go:build linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestForegroundFromEnvDefaultsToTheBuildsServerAndValidates(t *testing.T) {
	t.Cleanup(func(old string) func() { return func() { buildinfo.DefaultServerURL = old } }(buildinfo.DefaultServerURL))
	buildinfo.DefaultServerURL = "https://control.example.test/"
	config, err := foregroundFromEnv(envMap(map[string]string{"PAPERBOAT_ENROLLMENT_CREDENTIAL": " tok \n"}))
	if err != nil || config.controlURL != "https://control.example.test" || config.token != "tok" || config.machineID != "" {
		t.Fatalf("defaults = %+v, %v", config, err)
	}
	for name, values := range map[string]map[string]string{
		"http control":    {"PAPERBOAT_CONTROL_URL": "http://control.example.test"},
		"query":           {"PAPERBOAT_CONTROL_URL": "https://control.example.test/?x=1"},
		"other workspace": {"PAPERBOAT_WORKSPACE": "/home/x"},
	} {
		if _, err := foregroundFromEnv(envMap(values)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	buildinfo.DefaultServerURL = ""
	if _, err := foregroundFromEnv(envMap(nil)); err == nil {
		t.Fatal("a build with no default server and no PAPERBOAT_CONTROL_URL was accepted")
	}
}

func TestForegroundEnvironmentSetsAndDrops(t *testing.T) {
	got := foregroundEnvironment([]string{"A=1", "B=2", "SECRET=x", "NOTIFY_SOCKET=/s"}, map[string]string{"B": "3", "C": "4"}, "SECRET", "NOTIFY_SOCKET")
	joined := "," + strings.Join(got, ",") + ","
	for _, want := range []string{",A=1,", ",B=3,", ",C=4,"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("environment %v lacks %s", got, want)
		}
	}
	if strings.Contains(joined, "B=2") || strings.Contains(joined, "SECRET") || strings.Contains(joined, "NOTIFY_SOCKET") {
		t.Fatalf("environment %v kept a replaced or dropped key", got)
	}
}

func TestEnsureForegroundDirFixesModeAndRefusesSymlinksAndOtherOwners(t *testing.T) {
	root, uid, gid := t.TempDir(), os.Getuid(), os.Getgid()
	loose := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(loose, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ensureForegroundDir(loose, 0o700, uid, gid); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(loose); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	created := filepath.Join(root, "new", "dir")
	if err := ensureForegroundDir(created, 0o755, uid, gid); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(loose, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureForegroundDir(link, 0o700, uid, gid); err == nil {
		t.Fatal("a symlink was followed")
	}
	if os.Geteuid() != 0 {
		if err := ensureForegroundDir(loose, 0o700, uid+1, gid); err == nil {
			t.Fatal("a directory another account owns was accepted")
		}
	}
}

func TestExitStatusKeepsHostdsStatus(t *testing.T) {
	for want, script := range map[int]string{0: "exit 0", 7: "exit 7", 1: "kill -9 $$"} {
		if got := exitStatus(exec.Command("sh", "-c", script).Run()); got != want {
			t.Fatalf("%q exit status = %d, want %d", script, got, want)
		}
	}
}

// fakeHostd is a pb stand-in: superviseHostd runs it as `<exe> daemon __runtime-hostd`.
func fakeHostd(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pb")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuperviseHostdExitsWithHostdsOwnStatus(t *testing.T) {
	account, _ := user.Current()
	var out, errOut bytes.Buffer
	got := superviseHostd(context.Background(), fakeHostd(t, "echo ready; exit 7"), account, os.Environ(), &out, &errOut)
	if got != 7 || !strings.Contains(out.String(), "ready") {
		t.Logf("stderr: %s", errOut.String())
		t.Fatalf("status = %d, output %q", got, out.String())
	}
}

func TestSuperviseHostdStopSignalDrainsHostdAndExitsZero(t *testing.T) {
	account, _ := user.Current()
	marker := filepath.Join(t.TempDir(), "drained")
	script := fakeHostd(t, "trap 'echo drained > "+marker+"; exit 0' TERM\nwhile :; do sleep 0.05; done")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- superviseHostd(ctx, script, account, os.Environ(), io.Discard, io.Discard) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if _, err := os.Stat(marker); code != 0 || err != nil {
			t.Fatalf("status = %d, hostd drained: %v", code, err == nil)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor did not stop after the signal")
	}
}

func TestPairForegroundNeedsAToken(t *testing.T) {
	account, _ := user.Current()
	err := pairForeground(context.Background(), "/bin/true", account, t.TempDir(), foregroundConfig{}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "PAPERBOAT_ENROLLMENT_CREDENTIAL") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallBinaryOnlyCopiesThePbAndNothingElse(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "downloaded")
	if err := os.WriteFile(source, []byte("pb-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "bin")
	var stderr bytes.Buffer
	target, err := installBinaryOnly(source, directory, &stderr)
	if err != nil || target != filepath.Join(directory, "pb") {
		t.Fatalf("target = %q, %v", target, err)
	}
	if body, _ := os.ReadFile(target); string(body) != "pb-binary" {
		t.Fatalf("installed %q", body)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 1 {
		t.Fatalf("install left %d entries, want only pb", len(entries))
	}
	for _, want := range []string{"no systemd", target + " daemon --foreground", "PAPERBOAT_ENROLLMENT_CREDENTIAL"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("message %q lacks %q", stderr.String(), want)
		}
	}
	// Running the installed copy again must not rewrite itself.
	if again, err := installBinaryOnly(target, directory, &stderr); err != nil || again != target {
		t.Fatalf("reinstall = %q, %v", again, err)
	}
	if _, err := installBinaryOnly(source, "relative/dir", &stderr); err == nil {
		t.Fatal("a relative install directory was accepted")
	}
}

func TestOwnerEnvironmentCarriesNothingOfTheContainers(t *testing.T) {
	t.Setenv("PAPERBOAT_ENROLLMENT_CREDENTIAL", "secret-token")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret-key")
	t.Setenv("PATH", "/custom/bin")
	got := ownerEnvironment(&user.User{Username: "paperboat", HomeDir: "/workspace"}, map[string]string{"PAPERBOAT_SHELL": "/bin/sh"})
	joined := strings.Join(got, "\n")
	for _, secret := range []string{"secret-token", "secret-key", "ENROLLMENT"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("the owner environment carries %q: %v", secret, got)
		}
	}
	for _, want := range []string{"PATH=/custom/bin", "HOME=/workspace", "USER=paperboat", "PAPERBOAT_SHELL=/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
}

func TestForegroundOwnersKeepOneControllerPerGeneration(t *testing.T) {
	owners := &foregroundOwners{owners: map[string]*foregroundOwner{}}
	account := &user.User{Username: "paperboat", HomeDir: "/workspace"}
	d := sessionowner.Descriptor{Generation: strings.Repeat("a", 32), UID: 1, GID: 1, Executable: "/bin/true"}
	first, err := owners.controller(sessionowner.Catalog{Root: "/catalog"}, d, account, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := owners.controller(sessionowner.Catalog{Root: "/catalog"}, d, account, io.Discard, io.Discard); err != nil || second != first {
		t.Fatalf("a second request for the same generation made another controller (%v), so the first child would be lost", err)
	}
	owners.stop(context.Background())
	if _, err := owners.controller(sessionowner.Catalog{Root: "/catalog"}, d, account, io.Discard, io.Discard); err == nil {
		t.Fatal("a controller was handed out after stop, so a late start request could begin a child that outlives the supervisor")
	}
}

func TestForegroundOwnerStartsOnTheLatestPublishedExecutable(t *testing.T) {
	owners := &foregroundOwners{owners: map[string]*foregroundOwner{}}
	account := &user.User{Username: "paperboat", HomeDir: "/workspace"}
	d := sessionowner.Descriptor{Generation: strings.Repeat("a", 32), UID: 1, GID: 1, Executable: "/old/pb"}
	controller, err := owners.controller(sessionowner.Catalog{Root: "/catalog"}, d, account, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	d.Executable = "/new/pb"
	if _, err := owners.controller(sessionowner.Catalog{Root: "/catalog"}, d, account, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if command := controller.(*service.ProcessController).Command(); command.Path != "/new/pb" {
		t.Fatalf("a republished generation starts %q, not its latest executable", command.Path)
	}
}

func TestForegroundShellIsAnAbsolutePathWithoutSymlinks(t *testing.T) {
	shell := foregroundShell()
	if resolved, err := filepath.EvalSymlinks(shell); err != nil || resolved != shell {
		t.Fatalf("shell %q resolves to %q, %v", shell, resolved, err)
	}
}

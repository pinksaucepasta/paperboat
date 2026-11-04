//go:build linux || darwin

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// pbvm4 feat3/feat4: pb update refused ("hostd owner maintenance is busy") or
// would have cut a running exec, because commands ran inside hostd, which the
// update replaces. Commands now run in session owners: a command started
// through one hostd keeps running and streaming when that hostd goes away and
// a newer owner generation becomes active, and new commands use the new one.
func TestRunningExecSurvivesHostdReplacementAndNewOwnerGeneration(t *testing.T) {
	binary := os.Getenv("PAPERBOAT_OWNER_TEST_BINARY")
	if binary == "" || os.Geteuid() != 0 {
		t.Skip("requires built pb and privileged descriptor publication")
	}
	root, err := os.MkdirTemp("/var/lib", "paperboat-owner-exec-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(root, "pb")
	if err := os.WriteFile(pin, body, 0755); err != nil {
		t.Fatal(err)
	}
	source, err := installsource.Inspect(pin, "dev-owner-exec-test", installsource.Custom)
	if err != nil {
		t.Fatal(err)
	}
	catalogRoot := filepath.Join(root, "catalog")
	capability := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32))
	publish := func(generation, socket string, initial bool) sessionowner.Descriptor {
		d := sessionowner.Descriptor{Schema: sessionowner.Schema, Generation: generation, Instance: "exec-test", Source: source, Executable: pin, RuntimeRoot: filepath.Join(root, "state"), InitialStore: initial, Endpoint: filepath.Join(root, "ipc", socket), Workspace: root, Shell: "/bin/sh"}
		for _, path := range []string{d.PrivateRoot(), filepath.Dir(d.Endpoint), catalogRoot} {
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(d.TokenPath(), []byte(capability), 0600); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(catalogRoot, generation+".json"), encoded, 0644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	start := func(d sessionowner.Descriptor) {
		command := exec.Command(pin, "daemon", "__runtime-session-owner", "--generation", d.Generation)
		command.Env = []string{"PATH=/bin:/usr/bin", "PAPERBOAT_SESSION_OWNER_CATALOG=" + catalogRoot}
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		t.Cleanup(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-done
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	catalog := sessionowner.Catalog{Root: catalogRoot, ConfigRoot: "/", User: "root", Group: "root", Maximum: 21}
	noStart := func(context.Context, string) error { return session.ErrOwnerUnavailable }

	old := publish(source.SHA256[:32], "old.sock", true)
	start(old)
	oldClient, err := sessionowner.WaitReady(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	defer oldClient.Close()
	before, err := sessionowner.NewConnections(ctx, catalog, old, noStart, 20)
	if err != nil {
		t.Fatal(err)
	}
	executions, err := before.Executions()
	if err != nil {
		t.Fatal(err)
	}
	const id = "operation_exec_survives_update"
	if _, _, err := executions.Start(ctx, execprocess.Request{OperationID: id, Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 20 ]; do echo tick$i; i=$((i+1)); sleep 0.1; done; echo finished"}, CWD: root}); err != nil {
		t.Fatal(err)
	}
	// The hostd that started it is replaced: its connections go away.
	before.Close()

	next := publish(strings.Repeat("e", 32), "new.sock", false)
	start(next)
	nextClient, err := sessionowner.WaitReady(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	defer nextClient.Close()
	if err := oldClient.Retire(ctx); !errors.Is(err, session.ErrUpdateBusy) {
		t.Fatalf("owner with a running command retired: %v", err)
	}
	after, err := sessionowner.NewConnections(ctx, catalog, next, noStart, 20)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	routed, err := after.Executions()
	if err != nil {
		t.Fatal(err)
	}
	command, err := routed.Get(id)
	if err != nil {
		t.Fatalf("running command not found after hostd replacement: %v", err)
	}
	var output []byte
	from := uint64(1)
	for {
		event, err := command.Next(ctx, from)
		if err != nil {
			t.Fatalf("stream after hostd replacement: %v", err)
		}
		output = append(output, event.Data...)
		from = event.Sequence + 1
		if event.Result != nil {
			if event.Result.Code != 0 {
				t.Fatalf("command exit=%d", event.Result.Code)
			}
			break
		}
	}
	if !bytes.Contains(output, []byte("tick0")) || !bytes.Contains(output, []byte("finished")) {
		t.Fatalf("command output incomplete across replacement: %q", output)
	}

	// New commands start in the active generation.
	if _, _, err := routed.Start(ctx, execprocess.Request{OperationID: "operation_exec_new_generation", Argv: []string{"/bin/true"}, CWD: root}); err != nil {
		t.Fatal(err)
	}
	nextExec, err := nextClient.Executions()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nextExec.Get("operation_exec_new_generation"); err != nil {
		t.Fatalf("new command not in the active generation: %v", err)
	}
	if err := oldClient.Retire(ctx); err != nil {
		t.Fatalf("idle old owner did not retire: %v", err)
	}
}

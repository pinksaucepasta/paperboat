package containers

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type runtimeReadyWriter struct {
	once  sync.Once
	ready chan struct{}
}

func (w *runtimeReadyWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	return len(p), nil
}

type runtimeHandoffFixture struct {
	ctx                         context.Context
	dir, component, image, name string
}

func newRuntimeHandoffFixture(t *testing.T) runtimeHandoffFixture {
	t.Helper()
	image := os.Getenv("PAPERBOAT_TEST_DOCKER_BASE_IMAGE")
	if image == "" {
		t.Skip("explicit local Docker fixture image required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	t.Cleanup(cancel)
	dir := t.TempDir()
	component := filepath.Join(dir, "relay")
	if err := os.Mkdir(component, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/selfhost/runtime-readiness" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("c", 48) {
			w.WriteHeader(403)
			return
		}
		now := time.Now().UTC()
		json.NewEncoder(w).Encode(map[string]any{"data": NodeReadiness{NodeID: "fixture-node", Capability: "relay", Ready: true, ProcessEpoch: "fixture-epoch", ServerTime: now, ValidUntil: new(now.Add(time.Minute))}})
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(dir, "ca.pem")
	files := map[string][]byte{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), filepath.Join(component, "runtime.credential"): []byte(strings.Repeat("c", 48))}
	args, _ := json.Marshal([]string{"-node-id", "fixture-node", "-control-url", server.URL, "-control-ca", ca})
	files[filepath.Join(component, "runtime.args")] = args
	for path, b := range files {
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	owner := installationOwner(dir)
	name := "paperboat-pbh-" + owner + "-relay"
	if _, err := docker(ctx, "run", "-d", "--name", name, "--label", ownerLabel+"="+owner, "--entrypoint", "/bin/sleep", image, "120"); err != nil {
		t.Fatal("start owned fixture failed")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := removeOwned(cleanup, name, owner); err != nil {
			t.Error(err)
		}
	})
	return runtimeHandoffFixture{ctx: ctx, dir: dir, component: component, image: image, name: name}
}

func TestDockerSupervisorAdoptsUnchangedRuntimeWithoutRestart(t *testing.T) {
	f := newRuntimeHandoffFixture(t)
	ctx, dir, component, image, name := f.ctx, f.dir, f.component, f.image, f.name
	before, err := docker(ctx, "inspect", "--format", "{{.Id}}", name)
	if err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{StateDir: dir, Images: Images{Relay: image}}
	run, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	writer := &runtimeReadyWriter{ready: make(chan struct{})}
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { done <- launcher.Run(run, "relay", component, writer); close(finished) }()
	t.Cleanup(func() {
		cancelRun()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Error("supervisor fixture did not exit")
		}
	})
	select {
	case <-writer.ready:
	case err := <-done:
		t.Fatal("adopting an unchanged runtime stopped it", err)
	case <-ctx.Done():
		t.Fatal("runtime adoption did not become ready")
	}
	after, err := docker(ctx, "inspect", "--format", "{{.Id}}", name)
	if err != nil || string(after) != string(before) {
		t.Fatal("unchanged runtime was restarted during supervisor adoption")
	}
	preserved, err := PrepareHandoff(ctx, dir, Images{Relay: image})
	if err != nil || !preserved["relay"] {
		t.Fatal("unchanged role was not included in installer handoff", err)
	}
	cancelRun()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("handoff did not finish")
	}
	after, err = docker(ctx, "inspect", "--format", "{{.Id}}", name)
	if err != nil || string(after) != string(before) {
		t.Fatal("handoff terminated an unchanged runtime")
	}
	if err := ClearHandoff(dir); err != nil {
		t.Fatal(err)
	}
	// The next supervisor adopts the same process; an ordinary stop still
	// removes it rather than treating every shutdown as an upgrade.
	next, stopNext := context.WithCancel(ctx)
	nextDone := make(chan error, 1)
	nextFinished := make(chan struct{})
	nextWriter := &runtimeReadyWriter{ready: make(chan struct{})}
	go func() { nextDone <- launcher.Run(next, "relay", component, nextWriter); close(nextFinished) }()
	t.Cleanup(func() {
		stopNext()
		select {
		case <-nextFinished:
		case <-time.After(15 * time.Second):
			t.Error("next supervisor did not exit")
		}
	})
	select {
	case <-nextWriter.ready:
	case err := <-nextDone:
		t.Fatal("next supervisor failed adoption", err)
	case <-ctx.Done():
		t.Fatal("next supervisor readiness timed out")
	}
	stopNext()
	select {
	case err := <-nextDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("explicit stop did not finish")
	}

	if _, exists, err := inspectContainer(ctx, name); err != nil || exists {
		t.Fatal("explicit supervisor stop retained owned runtime")
	}
}

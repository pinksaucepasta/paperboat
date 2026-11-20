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
	"sync/atomic"
	"testing"
	"time"
)

type runtimeReadyWriter struct {
	once    sync.Once
	ready   chan struct{}
	release <-chan struct{}
}

func (w *runtimeReadyWriter) Write(p []byte) (int, error) {
	if !strings.HasSuffix(string(p), " container ready.\n") {
		return len(p), nil
	}
	w.once.Do(func() {
		close(w.ready)
		if w.release != nil {
			<-w.release
		}
	})
	return len(p), nil
}

type runtimeHandoffFixture struct {
	ctx                         context.Context
	dir, component, image, name string
}

func newRuntimeHandoffFixture(t *testing.T, dir, role string, failAfterFirst bool) runtimeHandoffFixture {
	t.Helper()
	image := os.Getenv("PAPERBOAT_TEST_DOCKER_BASE_IMAGE")
	if image == "" {
		t.Skip("explicit local Docker fixture image required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	component := filepath.Join(dir, role)
	if err := os.Mkdir(component, 0700); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/selfhost/runtime-readiness" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("c", 48) {
			w.WriteHeader(403)
			return
		}
		if calls.Add(1) > 1 && failAfterFirst {
			w.WriteHeader(503)
			return
		}
		now := time.Now().UTC()
		json.NewEncoder(w).Encode(map[string]any{"data": NodeReadiness{NodeID: "fixture-node", Capability: role, Ready: true, ProcessEpoch: "fixture-epoch", ServerTime: now, ValidUntil: new(now.Add(time.Minute))}})
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(component, "ca.pem")
	files := map[string][]byte{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), filepath.Join(component, "runtime.credential"): []byte(strings.Repeat("c", 48))}
	args, _ := json.Marshal([]string{"-node-id", "fixture-node", "-control-url", server.URL, "-control-ca", ca})
	files[filepath.Join(component, "runtime.args")] = args
	if role == "tunnel" {
		deployment, err := json.Marshal(map[string]string{"control_url": server.URL, "control_ca_file": ca})
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Join(component, "deployment.json")] = deployment
	}
	for path, b := range files {
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	owner := installationOwner(dir)
	name := containerName(owner, role)
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
	f := newRuntimeHandoffFixture(t, t.TempDir(), "relay", false)
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

func TestDockerAdoptionReadinessOutageRetainsRuntime(t *testing.T) {
	f := newRuntimeHandoffFixture(t, t.TempDir(), "relay", true)
	before, err := docker(f.ctx, "inspect", "--format", "{{.Id}}", f.name)
	if err != nil {
		t.Fatal(err)
	}
	launcher := Launcher{StateDir: f.dir, Images: Images{Relay: f.image}}
	err = launcher.Run(f.ctx, "relay", f.component, &runtimeReadyWriter{ready: make(chan struct{})})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("readiness outage did not retain its deadline error", err)
	}
	after, err := docker(f.ctx, "inspect", "--format", "{{.Id}}", f.name)
	if err != nil || string(after) != string(before) {
		t.Fatal("readiness outage terminated the admitted runtime")
	}
}

func TestDockerEdgeOnlyUpgradeAdoptsRelayAndHandsOverEdge(t *testing.T) {
	dir := t.TempDir()
	// The runtime drops DAC override. Allow its root user to traverse the bind
	// mount and write only the fixture's takeover signal.
	if err := os.Chmod(dir, 0711); err != nil {
		t.Fatal(err)
	}
	signal := filepath.Join(dir, "takeover")
	if err := os.WriteFile(signal, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(signal, 0666); err != nil {
		t.Fatal(err)
	}
	relay := newRuntimeHandoffFixture(t, dir, "relay", false)
	edge := newRuntimeHandoffFixture(t, dir, "tunnel", false)
	ctx := edge.ctx
	owner := installationOwner(dir)
	relayID, err := docker(ctx, "inspect", "--format", "{{.Id}}", relay.name)
	if err != nil {
		t.Fatal(err)
	}

	start := func(launcher Launcher, role, component string, holdReady bool) (context.CancelFunc, <-chan error) {
		run, cancel := context.WithCancel(ctx)
		writer := &runtimeReadyWriter{ready: make(chan struct{})}
		if holdReady {
			writer.release = run.Done()
		}
		done := make(chan error, 1)
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			done <- launcher.Run(run, role, component, writer)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-stopped:
			case <-time.After(15 * time.Second):
				t.Error("runtime did not stop")
			}
		})
		select {
		case <-writer.ready:
		case err := <-done:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return cancel, done
	}
	stop := func(cancel context.CancelFunc, done <-chan error) {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}

	old := Launcher{StateDir: dir, Images: Images{Relay: relay.image, Edge: edge.image}}
	stopRelay, relayDone := start(old, "relay", relay.component, false)
	stopEdge, edgeDone := start(old, "tunnel", edge.component, true)

	// A long-lived launcher can have a stale image snapshot. Swap the serving edge
	// under the same runtime name before the installer prepares the next upgrade.
	if err := removeOwned(ctx, edge.name, owner); err != nil {
		t.Fatal(err)
	}
	current := replacementImage(t, ctx, edge.image, "serving", "while [ ! -s "+signal+" ]; do sleep 0.1; done")
	if err := old.startContainer(ctx, "tunnel", current); err != nil {
		t.Fatal(err)
	}
	currentID, err := docker(ctx, "inspect", "--format", "{{.Id}}", edge.name)
	if err != nil {
		t.Fatal(err)
	}
	next := replacementImage(t, ctx, edge.image, "replacement", "echo ready > "+signal+" && sleep 120")
	images := Images{Relay: relay.image, Edge: next}
	preserve, err := PrepareHandoff(ctx, dir, images)
	if err != nil || !preserve["relay"] || !preserve["tunnel"] {
		t.Fatal("prepare edge-only handoff", err)
	}
	stop(stopEdge, edgeDone)
	stop(stopRelay, relayDone)
	if err := removeInstallationRuntimes(ctx, dir, true); err != nil {
		t.Fatal(err)
	}
	if after, err := docker(ctx, "inspect", "--format", "{{.Id}}", edge.name); err != nil || string(after) != string(currentID) {
		t.Fatal("supervisor shutdown stopped the serving edge before its replacement started")
	}
	if err := ClearHandoff(dir); err != nil {
		t.Fatal(err)
	}

	replacement := Launcher{StateDir: dir, Images: images}
	start(replacement, "relay", relay.component, false)
	// Run reports readiness after the replacement has started and the old
	// edge has retired in response to the takeover marker.
	start(replacement, "tunnel", edge.component, false)
	if after, err := docker(ctx, "inspect", "--format", "{{.Id}}", relay.name); err != nil || string(after) != string(relayID) {
		t.Fatal("edge-only upgrade restarted the relay")
	}
	if info, err := os.Stat(signal); err != nil || info.Size() == 0 {
		t.Fatal("replacement did not signal takeover", err)
	}
	want, err := resolvedImage(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	info, exists, err := inspectContainer(ctx, edge.name)
	if err != nil || !exists || !info.Running || info.ImageID != want {
		t.Fatal("edge replacement did not take over the serving name", err)
	}
}

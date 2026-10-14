package containers

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerTeardownPreservesForeignContainerAndRemovesOwnedContainer(t *testing.T) {
	image := os.Getenv("PAPERBOAT_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("configured local Docker integration image required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "paperboat-container-test-" + installationOwner(t.TempDir())
	owner := "integration-owner"
	if _, err := docker(ctx, "create", "--name", name, "--label", ownerLabel+"=foreign-owner", image, "--version"); err != nil {
		t.Fatal("create integration container failed")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		docker(cleanup, "rm", name)
	})
	if err := removeOwned(ctx, name, owner); err == nil {
		t.Fatal("foreign container was accepted as owned")
	}
	info, exists, err := inspectContainer(ctx, name)
	if err != nil || !exists || info.Owner != "foreign-owner" {
		t.Fatal("foreign container was changed or removed")
	}
	if _, err := docker(ctx, "rm", name); err != nil {
		t.Fatal("fixture removal failed")
	}
	if _, err := docker(ctx, "create", "--name", name, "--label", ownerLabel+"="+owner, image, "--version"); err != nil {
		t.Fatal("create owned integration container failed")
	}
	if err := removeOwned(ctx, name, owner); err != nil {
		t.Fatal("owned teardown failed")
	}
	_, exists, err = inspectContainer(ctx, name)
	if err != nil || exists {
		t.Fatal("owned stopped container survived teardown")
	}
}

func TestDockerResponseBufferBounds(t *testing.T) {
	var output cappedOutput
	payload := strings.Repeat("x", (1<<20)+1)
	if n, err := output.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatal("bounded writer broke command output delivery")
	}
	if !output.overflow || output.Len() != 1<<20 {
		t.Fatal("Docker output escaped its size bound")
	}
}

// Opt-in qualification uses the explicitly supplied claimed infrastructure
// installation. Its normal service must be stopped for this lifecycle check.
func TestLiveDockerSupervisorReadinessAndBoundedDrain(t *testing.T) {
	dir := os.Getenv("PAPERBOAT_TEST_PBH_STATE_DIR")
	if dir == "" {
		t.Skip("explicit claimed infrastructure installation required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	roles := []string{"relay", "tunnel"}
	previous := map[string]string{}
	for _, role := range roles {
		client, err := newNodeClient(role, filepath.Join(dir, role))
		if err != nil {
			t.Fatal("read node control configuration failed")
		}
		state, err := client.readiness(ctx)
		client.http.CloseIdleConnections()
		if err != nil {
			t.Fatal("read previous node epoch failed")
		}
		previous[role] = state.ProcessEpoch
	}
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, io.Discard, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("supervisor cleanup exceeded bound")
		}
	})
	ready := map[string]bool{}
	for len(ready) < len(roles) {
		for _, role := range roles {
			client, err := newNodeClient(role, filepath.Join(dir, role))
			if err != nil {
				t.Fatal("read launched node configuration failed")
			}
			state, err := client.readiness(ctx)
			client.http.CloseIdleConnections()
			if err == nil && state.Ready && state.ProcessEpoch != previous[role] {
				ready[role] = true
			}
		}
		select {
		case err := <-done:
			done <- err
			t.Fatal("supervisor exited before new authenticated readiness")
		case <-ctx.Done():
			t.Fatal("nodes did not reach readiness within the bound")
		case <-time.After(250 * time.Millisecond):
		}
	}
	owner := installationOwner(dir)
	for _, role := range roles {
		running, err := containerRunning(ctx, "paperboat-pbh-"+owner+"-"+role, owner)
		if err != nil || !running {
			t.Fatal("ready node did not have its owned running container")
		}
	}
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("supervisor shutdown returned a non-cancellation failure")
		}
		done <- err
	case <-time.After(15 * time.Second):
		t.Fatal("supervisor shutdown exceeded bound")
	}
	cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	for _, role := range roles {
		_, exists, err := inspectContainer(cleanup, "paperboat-pbh-"+owner+"-"+role)
		if err != nil || exists {
			t.Fatal("owned runtime container survived supervisor shutdown")
		}
	}
	t.Logf("real relay/tunnel readiness and current epochs verified; drain completed in %s", time.Since(started).Round(time.Millisecond))
}

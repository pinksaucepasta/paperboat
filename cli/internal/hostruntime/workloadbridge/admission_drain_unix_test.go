//go:build linux || darwin

package workloadbridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
)

// pbvm4 feat3 and a lone foreground tunnel: a long-lived feature stream (exec,
// SSH, transfer, preview) held its admission and every pb update failed with
// "hostd owner maintenance is busy". Maintenance must revoke it after the
// bounded drain, signal the holder, and succeed.
func TestLongFeatureStreamDoesNotBlockMaintenancePastTheDrainBound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := shorttemp.Dir(t)
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, MaximumActive: 1, MaximumOperations: 2, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer executions.Shutdown(context.Background())
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "worker", Version: "1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	ready := hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}
	if err := controller.MarkReady(ready); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Activate(hostdproto.Activate{WorkerID: ready.WorkerID, APIVersion: ready.APIVersion, Epoch: ready.Epoch, Lease: ready.Lease}); err != nil {
		t.Fatal(err)
	}
	token := make([]byte, 32)
	token[0] = 9
	server := &Server{Endpoint: filepath.Join(root, "ipc", "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 1}, Fence: controller.Status, AcquireActive: controller.AcquireActive, AcquireStream: controller.AcquireStream, MaximumConcurrent: 4, Ready: make(chan error, 1)}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := <-server.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		<-done
	}()
	client, err := NewClient(server.Endpoint, token, "worker", welcome.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	release, lost, err := client.AcquireAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// The installer's maintenance window is 35 s; the stream never ends.
	window, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	started := time.Now()
	if _, err := controller.PrepareMaintenance(window, false, nil); err != nil {
		t.Fatalf("a held feature stream refused maintenance: %v", err)
	}
	if took := time.Since(started); took < hostdproto.StreamDrainBound || took > hostdproto.StreamDrainBound+2*time.Second {
		t.Fatalf("maintenance took %v, want about the %v drain bound", took, hostdproto.StreamDrainBound)
	}
	select {
	case <-lost:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked stream holder was not told to stop")
	}
}

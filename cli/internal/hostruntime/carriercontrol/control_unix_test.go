//go:build linux || darwin

package carriercontrol

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

func TestPrivateCarrierRetirementPreservesAcceptedConnection(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	const sourceA = "1111111111111111111111111111111111111111111111111111111111111111"
	const sourceB = "2222222222222222222222222222222222222222222222222222222222222222"
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	if _, err := ssh.ReconcileSourceTarget(context.Background(), 1, port, sourceA); err != nil {
		t.Fatal(err)
	}
	controller, workerID, workerEpoch := activeControlWorker(t)
	lost := make(chan struct{})
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(root, "control.sock")
	token := []byte(strings.Repeat("c", 32))
	sha := strings.Repeat("a", 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Endpoint: endpoint, Token: token, SHA256: sha, SSH: ssh, WorkerFence: func(_ context.Context, id string, epoch uint64) (func(), <-chan struct{}, error) {
		release, err := controller.AcquireActive(id, epoch)
		return release, lost, err
	}, Ready: make(chan error, 1)}
	var publishNewTarget atomic.Bool
	server.RefreshTarget = func(ctx context.Context) error {
		if !publishNewTarget.Load() {
			return nil
		}
		_, err := ssh.ReconcileSourceTarget(ctx, 2, port, sourceB)
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-server.Ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("carrier control not ready")
	}
	client, err := NewClient(endpoint, token, sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if open, err := client.OpenSSH(ctx); err != nil || open != 0 {
		t.Fatalf("idle carrier open SSH = %d, %v", open, err)
	}
	workerClient, err := client.BindWorker(workerID, workerEpoch)
	if err != nil {
		t.Fatal(err)
	}
	wrongSource, _ := NewClient(endpoint, token, strings.Repeat("b", 64))
	wrongToken, _ := NewClient(endpoint, []byte(strings.Repeat("x", 32)), sha)
	wrongWorker, err := client.BindWorker("other-worker", workerEpoch)
	if err != nil {
		t.Fatal(err)
	}
	staleWorker, err := client.BindWorker(workerID, workerEpoch+1)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer left.Close()
	bridgeDone := make(chan error, 1)
	go func() { _, err := ssh.Serve(ctx, 1, right); bridgeDone <- err }()
	if err := left.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	echo := func() {
		t.Helper()
		if _, err := left.Write([]byte("alive")); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 5)
		if _, err := io.ReadFull(left, body); err != nil || string(body) != "alive" {
			t.Fatalf("SSH echo=%q error=%v", body, err)
		}
	}
	echo()
	// An update asks this before it starts: the incoming connection must be counted.
	if open, err := client.OpenSSH(ctx); err != nil || open != 1 {
		t.Fatalf("open SSH with one bridge = %d, %v", open, err)
	}
	if _, err := wrongSource.OpenSSH(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong source open SSH: %v", err)
	}
	if err := wrongSource.RetireSSH(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong source retirement: %v", err)
	}
	echo()
	if err := wrongToken.RetireSSH(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong capability retirement: %v", err)
	}
	echo()
	if err := wrongWorker.DisableSSH(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong worker disable: %v", err)
	}
	echo()
	if err := staleWorker.DisableSSH(ctx); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale epoch disable: %v", err)
	}
	echo()
	if err := client.RetireSSH(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("active retirement: %v", err)
	}
	echo()
	if err := workerClient.DisableSSH(ctx); err != nil {
		t.Fatalf("explicit SSH disable: %v", err)
	}
	select {
	case err := <-bridgeDone:
		_ = err // The explicit capability change cancels the accepted bridge.
	case <-time.After(3 * time.Second):
		t.Fatal("explicit SSH disable did not close the accepted bridge")
	}
	if _, ok := ssh.Target(); ok {
		t.Fatal("explicit SSH disable left target admission open")
	}
	if err := workerClient.EnableSSH(ctx); err != nil {
		t.Fatalf("explicit SSH enable: %v", err)
	}
	if _, ok := ssh.Target(); !ok {
		t.Fatal("explicit SSH enable did not restore admission")
	}
	if err := client.RetireSSH(ctx); err != nil {
		t.Fatal(err)
	}
	if err := workerClient.EnableSSH(ctx); err != nil {
		t.Fatalf("enable while retired: %v", err)
	}
	if _, ok := ssh.Target(); ok {
		t.Fatal("worker enable crossed the independent retirement fence")
	}
	if err := client.CheckSourceRetirement(ctx, sourceA); !errors.Is(err, ErrFenced) {
		t.Fatalf("cannot retire current source while SSH admission is retired: %v", err)
	}
	if err := client.ResumeSSH(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := ssh.Target(); !ok {
		t.Fatal("aborted replacement did not reopen admission")
	}
	if err := workerClient.DisableSSH(ctx); err != nil {
		t.Fatalf("disable idle SSH target: %v", err)
	}
	if err := client.CheckSourceRetirement(ctx, sourceA); !errors.Is(err, ErrFenced) {
		t.Fatalf("cannot retire current source while SSH is disabled: %v", err)
	}
	if err := workerClient.EnableSSH(ctx); err != nil {
		t.Fatalf("re-enable idle SSH target: %v", err)
	}
	if err := client.CheckSourceRetirement(ctx, sourceA); !errors.Is(err, ErrFenced) {
		t.Fatalf("cannot retire current source after old flow ended: %v", err)
	}
	// Retirement refreshes the authoritative current worker even when no new SSH
	// connection has arrived to publish its target.
	publishNewTarget.Store(true)
	if err := client.CheckSourceRetirement(ctx, sourceA); err != nil {
		t.Fatalf("retired source after source publication: %v", err)
	}
	if err := client.CheckSourceRetirement(ctx, sourceB); !errors.Is(err, ErrFenced) {
		t.Fatalf("retiring current source: %v", err)
	}
	if err := client.CheckSourceRetirement(ctx, "not-a-source-hash"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid source generation: %v", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := client.Health(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled health: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("carrier control did not stop")
	}
	if _, err := os.Lstat(endpoint); !os.IsNotExist(err) {
		t.Fatalf("owned socket retained: %v", err)
	}
}

func activeControlWorker(t *testing.T) (*hostdproto.Controller, string, uint64) {
	t.Helper()
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	const workerID = "worker-current"
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: workerID, Version: "test", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	ready := hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}
	if err := controller.MarkReady(ready); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Activate(hostdproto.Activate{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
		t.Fatal(err)
	}
	return controller, workerID, welcome.Epoch
}

type blockingCloseStream struct {
	io.ReadWriteCloser
	started chan struct{}
	proceed <-chan struct{}
	once    sync.Once
}

func (s *blockingCloseStream) Close() error {
	s.once.Do(func() {
		close(s.started)
		<-s.proceed
	})
	return s.ReadWriteCloser.Close()
}

func TestDisableSSHCancelsOnWorkerLeaseLossAndJoinsWatcher(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	ssh, err := managedssh.NewHost(managedssh.HostConfig{ProbeTimeout: time.Second, DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ReconcileTarget(context.Background(), 1, uint16(listener.Addr().(*net.TCPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	controller, workerID, workerEpoch := activeControlWorker(t)
	lost := make(chan struct{})
	releaseReturned := make(chan struct{})
	root := shorttemp.Dir(t)
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	endpoint := filepath.Join(root, "control.sock")
	token := []byte(strings.Repeat("d", 32))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{Endpoint: endpoint, Token: token, SHA256: strings.Repeat("c", 64), SSH: ssh, WorkerFence: func(_ context.Context, id string, epoch uint64) (func(), <-chan struct{}, error) {
		release, err := controller.AcquireActive(id, epoch)
		if err != nil {
			return nil, nil, err
		}
		return func() {
			release()
			close(releaseReturned)
		}, lost, nil
	}, Ready: make(chan error, 1)}
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	select {
	case err := <-server.Ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("carrier control not ready")
	}
	client, err := NewClient(endpoint, token, server.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	workerClient, err := client.BindWorker(workerID, workerEpoch)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	proceed := make(chan struct{})
	var proceedOnce sync.Once
	allowClose := func() { proceedOnce.Do(func() { close(proceed) }) }
	defer allowClose()
	stream := &blockingCloseStream{ReadWriteCloser: right, started: make(chan struct{}), proceed: proceed}
	bridgeDone := make(chan error, 1)
	go func() { _, err := ssh.Serve(ctx, 1, stream); bridgeDone <- err }()
	if err := left.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := left.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 5)
	if _, err := io.ReadFull(left, body); err != nil || string(body) != "ready" {
		t.Fatalf("SSH echo=%q error=%v", body, err)
	}
	disableDone := make(chan error, 1)
	go func() { disableDone <- workerClient.DisableSSH(ctx) }()
	select {
	case <-stream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker disable did not cancel the accepted bridge")
	}
	close(lost)
	select {
	case err := <-disableDone:
		if !errors.Is(err, ErrFenced) {
			t.Fatalf("lease loss disable error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker disable ignored lease loss")
	}
	select {
	case <-releaseReturned:
	case <-time.After(time.Second):
		t.Fatal("worker fence was not released after joining lease watcher")
	}
	select {
	case err := <-bridgeDone:
		t.Fatalf("bridge finished before its blocked close was released: %v", err)
	default:
	}
	allowClose()
	select {
	case <-bridgeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SSH bridge did not finish after the blocked close was released")
	}
	_ = left.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("carrier control did not stop")
	}
}

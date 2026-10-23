//go:build darwin || linux

package hostdproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestSocketLifecyclePersistsFenceAndRejectsSupersededWorker(t *testing.T) {
	config := testSocketConfig(t)
	config.Workloads = func() WorkloadStatus { return WorkloadStatus{Generation: 7, Protected: 3} }
	// Native service restart may remove its entire ephemeral socket directory.
	// The epoch fence belongs to the private persistent installation directory.
	runtimeState := t.TempDir()
	if err := os.Chmod(runtimeState, 0o755); err != nil {
		t.Fatal(err)
	}
	config.StatePath = filepath.Join(runtimeState, "hostd", "fence.json")
	server, cancel, done := startSocketServer(t, config)
	client := testSocketClient(t, config)

	first := negotiateSocket(t, client, "runtime-old")
	if _, err := client.Request(context.Background(), readyFor(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(first)); err != nil {
		t.Fatal(err)
	}
	if err := clientHeartbeat(client, first); err != nil {
		t.Fatalf("first heartbeat: %v", err)
	}
	second := negotiateSocket(t, client, "runtime-new")
	if _, err := client.Request(context.Background(), readyFor(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(second)); err != nil {
		t.Fatal(err)
	}
	if err := clientHeartbeat(client, first); !errors.Is(err, ErrFenced) {
		t.Fatalf("old heartbeat error=%v, want ErrFenced", err)
	}
	if err := clientHeartbeat(client, second); err != nil {
		t.Fatalf("new heartbeat: %v", err)
	}
	status, err := client.Active(context.Background())
	if err != nil || status.State != StateActive || status.WorkerID != "runtime-new" || status.Epoch != second.Epoch || status.WorkloadGeneration != 7 || status.ProtectedWorkloads != 3 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if info, err := os.Stat(config.SocketPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions info=%v err=%v", info, err)
	}
	if got := server.Status(); got.State != StateActive || got.Epoch != second.Epoch || got.WorkerID != "runtime-new" {
		t.Fatalf("server status=%+v", got)
	}
	stopSocketServer(t, cancel, done)

	state, err := LoadFenceState(config.StatePath)
	if err != nil || state.Epoch != 2 || state.WorkerID != "runtime-new" {
		t.Fatalf("fence state=%+v err=%v", state, err)
	}
	if err := os.RemoveAll(filepath.Dir(config.SocketPath)); err != nil {
		t.Fatal(err)
	}
	_, cancel, done = startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	restarted := testSocketClient(t, config)
	next := negotiateSocket(t, restarted, "runtime-next")
	if next.Epoch != second.Epoch+1 {
		t.Fatalf("restart epoch=%d, want %d", next.Epoch, second.Epoch+1)
	}
	if err := clientHeartbeat(restarted, second); !errors.Is(err, ErrFenced) {
		t.Fatalf("old lease survived native restart: %v", err)
	}
}

func TestSocketRejectsUnauthorizedUIDAndCapability(t *testing.T) {
	config := testSocketConfig(t)
	config.peerUID = func(*net.UnixConn) (int, error) { return config.UID + 1, nil }
	_, cancel, done := startSocketServer(t, config)
	client := testSocketClient(t, config)
	if _, err := client.Request(context.Background(), Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err == nil {
		t.Fatal("unauthorized UID request unexpectedly succeeded")
	}
	stopSocketServer(t, cancel, done)

	config = testSocketConfig(t)
	_, cancel, done = startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	wrongToken := append([]byte(nil), config.Token...)
	wrongToken[0] ^= 0xff
	client, err := NewClient(config.SocketPath, wrongToken, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err == nil {
		t.Fatal("invalid capability request unexpectedly succeeded")
	}
}

func TestServerCopiesInstallationCapabilityAtConstruction(t *testing.T) {
	config := testSocketConfig(t)
	client := testSocketClient(t, config)
	server, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	if strings.Contains(server.String(), config.SocketPath) {
		t.Fatal("server description exposed the private socket path")
	}
	config.Token[0] ^= 0xff
	if _, err := client.Request(context.Background(), Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err != nil {
		t.Fatalf("constructor-owned capability changed with caller memory: %v", err)
	}
}

func TestClientCancellationClosesEstablishedRequest(t *testing.T) {
	config := testSocketConfig(t)
	if err := os.MkdirAll(filepath.Dir(config.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: config.SocketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := NewClient(config.SocketPath, config.Token, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := client.Request(ctx, Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1})
		result <- err
	}()
	connection, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	token := make([]byte, installationTokenBytes)
	if _, err := io.ReadFull(connection, token); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(token, config.Token) {
		t.Fatal("client did not send its configured capability")
	}
	if _, err := ReadFrame(connection); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request error=%v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), config.SocketPath) {
			t.Fatalf("socket path escaped in request error: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		_ = connection.Close()
		select {
		case <-result:
		case <-time.After(time.Second):
		}
		t.Fatal("client did not close an established request on cancellation")
	}
}

type cancelAwareWorkerControl struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func (h cancelAwareWorkerControl) HandleWorkerControl(ctx context.Context, _ WorkerControlRequest) (WorkerControlResponse, error) {
	close(h.started)
	select {
	case <-ctx.Done():
		close(h.canceled)
		return WorkerControlResponse{Completed: true}, ctx.Err()
	case <-h.release:
		return WorkerControlResponse{Completed: true}, nil
	}
}

func TestSocketRunCancellationCancelsAndJoinsActiveRequest(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	config := testSocketConfig(t)
	config.RequestTimeout = 10 * time.Second
	config.WorkerControl = cancelAwareWorkerControl{started: started, canceled: canceled, release: release}
	_, cancel, done := startSocketServer(t, config)
	serverExited := false
	t.Cleanup(func() {
		cancel()
		if !serverExited {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("socket server cleanup did not finish")
			}
		}
	})
	client := testSocketClient(t, config)
	clientDone := make(chan error, 1)
	go func() {
		_, err := client.Request(context.Background(), WorkerControlRequest{Operation: "stop_active"})
		clientDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker-control callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		serverExited = true
		if err != nil {
			t.Fatalf("server stop: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		close(release)
		select {
		case <-done:
			serverExited = true
		case <-time.After(time.Second):
			t.Fatal("server remained stuck after releasing callback")
		}
		t.Fatal("server did not stop after canceling an active callback")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("run cancellation did not reach the active request context")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("active client request did not observe closed connection")
	}
}

type failingWorkerControl struct{ err error }

func (h failingWorkerControl) HandleWorkerControl(context.Context, WorkerControlRequest) (WorkerControlResponse, error) {
	return WorkerControlResponse{Completed: true}, h.err
}

type panickingUnwrapError struct{}

func (panickingUnwrapError) Error() string { return "private worker failure" }
func (panickingUnwrapError) Unwrap() error { panic("private unwrap detail") }

func TestSocketReportsAuthenticatedOperationalFailureOnceAndRecovers(t *testing.T) {
	const privateDetail = "private worker path /var/lib/paperboat/worker"
	failure := fmt.Errorf("%s: %w", privateDetail, syscall.EIO)
	config := testSocketConfig(t)
	config.WorkerControl = failingWorkerControl{err: failure}
	_, cancel, done := startSocketServer(t, config)
	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	client := testSocketClient(t, config)
	if _, err := client.Request(context.Background(), WorkerControlRequest{Operation: "stop_active"}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("private worker failure was not projected to the fixed protocol error: %v", err)
	}
	var fault errorreport.Fault
	select {
	case fault = <-faults:
	case <-time.After(time.Second):
		t.Fatal("authenticated operational failure was not observed")
	}
	if fault.Component != "paperboat-daemon" || fault.Operation != "machine_control" || fault.Stage != "control_request" || fault.Code != "control_request_failed" || fault.Cause != "system_error" || fault.Errno != int(syscall.EIO) || !supportref.Valid(fault.SupportReference) {
		t.Fatalf("fault lost safe classification or request reference: %+v", fault)
	}
	if strings.Contains(fmt.Sprintf("%+v", fault), privateDetail) {
		t.Fatal("fault exposed the worker's private path")
	}
	select {
	case duplicate := <-faults:
		t.Fatalf("request failure was observed more than once: %+v", duplicate)
	default:
	}
	if _, err := requestSocket(t, client, Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err != nil {
		t.Fatalf("healthy request did not recover after operational failure: %v", err)
	}
	stopSocketServer(t, cancel, done)
}

func TestSocketProjectsMalformedAuthenticatedFailureAndRecovers(t *testing.T) {
	config := testSocketConfig(t)
	config.WorkerControl = failingWorkerControl{err: panickingUnwrapError{}}
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	if _, err := client.Request(context.Background(), WorkerControlRequest{Operation: "stop_active"}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("malformed worker failure was not projected to the fixed protocol error: %v", err)
	}
	if _, err := requestSocket(t, client, Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err != nil {
		t.Fatalf("healthy request did not recover after malformed worker failure: %v", err)
	}
}

type releasedWorkerControl struct {
	started chan struct{}
	release chan struct{}
}

func (h releasedWorkerControl) HandleWorkerControl(ctx context.Context, _ WorkerControlRequest) (WorkerControlResponse, error) {
	close(h.started)
	select {
	case <-ctx.Done():
		return WorkerControlResponse{}, ctx.Err()
	case <-h.release:
	}
	return WorkerControlResponse{Completed: true}, nil
}

func TestSocketReportsAuthenticatedResponseWriteFailure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	config := testSocketConfig(t)
	config.WorkerControl = releasedWorkerControl{started: started, release: release}
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	faults := make(chan errorreport.Fault, 1)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	connection := rawSocketConnection(t, config.SocketPath)
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		t.Fatal("local socket did not return a Unix connection")
	}
	if err := writeAll(unixConnection, config.Token); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(unixConnection, WorkerControlRequest{Operation: "stop_active"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("authenticated worker-control request did not start")
	}
	if err := unixConnection.Close(); err != nil {
		t.Fatal("could not close the test client socket")
	}
	close(release)
	select {
	case fault := <-faults:
		if fault.Operation != "machine_control" || fault.Stage != "control_request" || fault.Code != "control_request_failed" || fault.Errno == 0 || fault.Cause != "broken_pipe" && fault.Cause != "connection_reset" {
			t.Fatalf("response-write failure lost native classification: %+v", fault)
		}
	case <-time.After(time.Second):
		t.Fatal("authenticated response-write failure was not observed")
	}
	if _, err := requestSocket(t, testSocketClient(t, config), Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err != nil {
		t.Fatalf("server did not recover after failed response delivery: %v", err)
	}
}

func TestSocketAllowsRootUpdaterOnlyWithCapability(t *testing.T) {
	config := testSocketConfig(t)
	config.peerUID = func(*net.UnixConn) (int, error) { return 0, nil }
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	if _, err := client.Request(context.Background(), Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err != nil {
		t.Fatalf("root updater request: %v", err)
	}
}

type fixedUpdateGate struct{ target UpdateGateTargetBinding }

func (g fixedUpdateGate) HandleUpdateGate(context.Context, UpdateGateRequest) (UpdateGateResponse, error) {
	return UpdateGateResponse{Target: g.target}, nil
}

func TestSocketRoundTripsUpdateGateResponse(t *testing.T) {
	config := testSocketConfig(t)
	config.UpdateGate = fixedUpdateGate{target: UpdateGateTargetBinding{
		Scope: UpdateGateScopeStandalone, MachineID: "machine_01", FailureDomain: "standalone",
	}}
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	response, err := client.UpdateGate(context.Background(), UpdateGateRequest{
		Operation: UpdateGateTarget, TransactionID: "transaction_01", Version: "2026.09.02.6", ManifestSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Target != config.UpdateGate.(fixedUpdateGate).target {
		t.Fatalf("target=%+v", response.Target)
	}
}

func TestSocketRepliesWithTypedErrorsAndBoundsRequests(t *testing.T) {
	config := testSocketConfig(t)
	config.RequestTimeout = 100 * time.Millisecond
	// startSocketServer proves accept-loop readiness with a short-lived
	// connection. Leave a second slot so that probe cannot transiently consume
	// the only admission while this test opens its real request.
	config.MaxConcurrent = 2
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)

	connection := rawSocketConnection(t, config.SocketPath)
	defer connection.Close()
	if err := writeAll(connection, config.Token); err != nil {
		t.Fatal(err)
	}
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], MaxFrameBytes+1)
	if err := writeAll(connection, oversized[:]); err != nil {
		t.Fatal(err)
	}
	response, err := ReadFrame(connection)
	if err != nil {
		t.Fatal(err)
	}
	remote, ok := response.(*Error)
	if !ok || remote.Code != "invalid" {
		t.Fatalf("malformed response=%+v", response)
	}
	// Each connection accepts one request. A second request cannot receive a
	// second reply from the same authenticated connection.
	if err := WriteFrame(connection, Hello{WorkerID: "runtime", Version: "1", APIMin: 1, APIMax: 1}); err == nil {
		if _, err := ReadFrame(connection); err == nil {
			t.Fatal("second request unexpectedly received a reply")
		}
	}

	blocking := rawSocketConnection(t, config.SocketPath)
	defer blocking.Close()
	if err := writeAll(blocking, config.Token); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = io.ReadFull(blocking, make([]byte, 1))
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("unterminated request error=%v elapsed=%s", err, time.Since(started))
	}
}

func TestSocketClientMapsRemoteFencingError(t *testing.T) {
	config := testSocketConfig(t)
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	first := negotiateSocket(t, client, "runtime-a")
	if _, err := client.Request(context.Background(), readyFor(first)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(first)); err != nil {
		t.Fatal(err)
	}
	second := negotiateSocket(t, client, "runtime-b")
	if _, err := client.Request(context.Background(), readyFor(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), heartbeatFor(first)); !errors.Is(err, ErrFenced) {
		t.Fatalf("fenced response error=%v, want ErrFenced", err)
	}
}

func TestCandidatePerformsReadyActivateAndFencesPriorWorker(t *testing.T) {
	config := testSocketConfig(t)
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	first, err := NewCandidate(client, "runtime-old", "2026.08.18.1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidateReady(t, first); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := NewCandidate(client, "runtime-new", "2026.08.18.2", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := candidateReady(t, second); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Heartbeat(context.Background()); !errors.Is(err, ErrFenced) {
		t.Fatalf("old worker heartbeat=%v", err)
	}
	if err := second.Heartbeat(context.Background()); err != nil {
		t.Fatalf("active worker heartbeat=%v", err)
	}
}

func candidateReady(t *testing.T, candidate *Candidate) (Status, error) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status, err := candidate.Ready(context.Background())
		var operation *net.OpError
		if err == nil || !errors.As(err, &operation) {
			return status, err
		}
		if time.Now().After(deadline) {
			return Status{}, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testSocketConfig(t *testing.T) SocketConfig {
	t.Helper()
	// Darwin's AF_UNIX path limit is much shorter than the normal Go test temp
	// directory path. Use an explicitly short root on every supported platform.
	root, err := os.MkdirTemp("/tmp", "pbhd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	random := make([]byte, 32*16)
	for index := range random {
		random[index] = byte(index/32 + 1)
	}
	return SocketConfig{
		SocketPath: filepath.Join(root, "socket", "hostd.sock"), StatePath: filepath.Join(root, "state", "fence.json"),
		UID: os.Geteuid(), GID: os.Getegid(), Token: bytes.Repeat([]byte{0x42}, installationTokenBytes),
		APIMin: 1, APIMax: 2, Random: bytes.NewReader(random),
	}
}

func startSocketServer(t *testing.T, config SocketConfig) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	server, err := NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		if info, err := os.Lstat(config.SocketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			// A stale pathname can be visible for the few instructions between
			// bind and the accept loop becoming reachable. Prove readiness with a
			// real connection instead of treating directory visibility as ready.
			connection, dialErr := net.DialTimeout("unix", config.SocketPath, 50*time.Millisecond)
			if dialErr == nil {
				_ = connection.Close()
				return server, cancel, done
			}
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("socket server exited before readiness: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("socket %s was not created", config.SocketPath)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func requestSocket(t *testing.T, client *Client, request Message) (Message, error) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		response, err := client.Request(context.Background(), request)
		var operation *net.OpError
		if err == nil || !errors.As(err, &operation) {
			return response, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stopSocketServer(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("socket server did not stop")
	}
}

func testSocketClient(t *testing.T, config SocketConfig) *Client {
	t.Helper()
	client, err := NewClient(config.SocketPath, config.Token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func negotiateSocket(t *testing.T, client *Client, workerID string) Welcome {
	t.Helper()
	response, err := requestSocket(t, client, Hello{WorkerID: workerID, Version: "2026.08.18.3", APIMin: 1, APIMax: 2})
	if err != nil {
		t.Fatal(err)
	}
	welcome, ok := response.(*Welcome)
	if !ok {
		t.Fatalf("hello response=%T", response)
	}
	return *welcome
}

func clientHeartbeat(client *Client, welcome Welcome) error {
	_, err := client.Request(context.Background(), heartbeatFor(welcome))
	return err
}

func rawSocketConnection(t *testing.T, path string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	var connection net.Conn
	var err error
	for {
		connection, err = net.DialTimeout("unix", path, 50*time.Millisecond)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	return connection
}

func TestSocketNativeCommitReleasesOnlyProtectedStartupFence(t *testing.T) {
	for _, operation := range []string{"native_commit", "gate_commit"} {
		t.Run(operation, func(t *testing.T) {
			config := testSocketConfig(t)
			var committed, gateValid atomic.Bool
			config.MaintenancePending = true
			config.VerifyMaintenanceCommitted = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !committed.Load() {
					return ErrMaintenanceBusy
				}
				return nil
			}
			target := UpdateGateTargetBinding{Scope: UpdateGateScopeStandalone, MachineID: "machine_01", FailureDomain: "standalone"}
			// A prior committed worker receipt may be replayed successfully. It
			// cannot release this startup's fence before its native source commits.
			config.UpdateGate = startupCommitGate{target: target, valid: &gateValid}
			server, cancel, done := startSocketServer(t, config)
			defer stopSocketServer(t, cancel, done)
			client := testSocketClient(t, config)
			worker := negotiateSocket(t, client, "candidate")
			if _, err := client.Request(context.Background(), readyFor(worker)); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Request(context.Background(), activateFor(worker)); err != nil {
				t.Fatalf("pending candidate could not become healthy: %v", err)
			}
			complete := func() error {
				if operation == "native_commit" {
					return client.CompleteMaintenance(context.Background())
				}
				_, err := client.UpdateGate(context.Background(), UpdateGateRequest{Operation: UpdateGateCommit, TransactionID: "prior_committed", Version: "2026.10.13.1", ManifestSHA256: strings.Repeat("a", 64), ExpectedTarget: &target})
				return err
			}
			if err := complete(); !errors.Is(err, ErrMaintenanceBusy) {
				t.Fatalf("uncommitted startup proof accepted: %v", err)
			}
			if err := client.AbortMaintenance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := server.AcquireActive(worker.WorkerID, worker.Epoch); !errors.Is(err, ErrMaintenanceBusy) {
				t.Fatalf("failed proof/abort released startup admission: %v", err)
			}
			if _, err := client.Request(context.Background(), Status{State: StateEmpty}); err != nil {
				t.Fatalf("pending candidate stopped answering status: %v", err)
			}
			committed.Store(true)
			if operation == "gate_commit" {
				if err := complete(); !errors.Is(err, ErrInvalidFrame) {
					t.Fatalf("malformed owner receipt released startup: %v", err)
				}
				if _, err := server.AcquireActive(worker.WorkerID, worker.Epoch); !errors.Is(err, ErrMaintenanceBusy) {
					t.Fatalf("malformed owner receipt bypassed fence: %v", err)
				}
			}
			gateValid.Store(true)
			if err := complete(); err != nil {
				t.Fatalf("durably committed startup failed release: %v", err)
			}
			release, err := server.AcquireActive(worker.WorkerID, worker.Epoch)
			if err != nil {
				t.Fatalf("same native fence did not reopen: %v", err)
			}
			release()
			if err := complete(); err != nil {
				t.Fatalf("committed cleanup retry failed: %v", err)
			}
		})
	}
}

type startupCommitGate struct {
	target UpdateGateTargetBinding
	valid  *atomic.Bool
}

func (g startupCommitGate) HandleUpdateGate(context.Context, UpdateGateRequest) (UpdateGateResponse, error) {
	if !g.valid.Load() {
		return UpdateGateResponse{}, nil
	}
	return UpdateGateResponse{Target: g.target}, nil
}

func TestSocketCanceledNativeCompletionLeavesStartupFence(t *testing.T) {
	config := testSocketConfig(t)
	config.MaintenancePending = true
	var canCommit atomic.Bool
	entered, stopped := make(chan struct{}), make(chan struct{})
	config.VerifyMaintenanceCommitted = func(ctx context.Context) error {
		if canCommit.Load() {
			return nil
		}
		close(entered)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	server, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	worker := negotiateSocket(t, client, "candidate")
	if _, err := client.Request(context.Background(), readyFor(worker)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), activateFor(worker)); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- client.CompleteMaintenance(ctx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("commit proof did not start")
	}
	stop()
	if err := <-result; err == nil {
		t.Fatal("canceled native completion succeeded")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("peer cancellation did not stop commit verifier")
	}
	if _, err := server.AcquireActive(worker.WorkerID, worker.Epoch); !errors.Is(err, ErrMaintenanceBusy) {
		t.Fatalf("canceled proof cleared startup fence: %v", err)
	}
	canCommit.Store(true)
	if err := client.CompleteMaintenance(context.Background()); err != nil {
		t.Fatalf("matching retry did not recover: %v", err)
	}
}

// An update asks the worker for the managed SSH connections it bridges itself before it starts.
func TestSocketReportsWorkerOpenSSHConnections(t *testing.T) {
	config := testSocketConfig(t)
	var open atomic.Int64
	open.Store(2)
	config.OpenSSH = func(context.Context) (int, error) { return int(open.Load()), nil }
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	client := testSocketClient(t, config)
	if got, err := client.OpenSSH(context.Background()); err != nil || got != 2 {
		t.Fatalf("open SSH = %d, %v", got, err)
	}
	open.Store(0)
	if got, err := client.OpenSSH(context.Background()); err != nil || got != 0 {
		t.Fatalf("idle open SSH = %d, %v", got, err)
	}
	if (WorkerControlResponse{Completed: true, OpenSSH: new(int), Status: &Status{}}).validate() == nil {
		t.Fatal("an open_ssh answer carrying a status was accepted")
	}
}

func TestSocketWithoutOpenSSHCountRefusesTheQuery(t *testing.T) {
	config := testSocketConfig(t)
	_, cancel, done := startSocketServer(t, config)
	defer stopSocketServer(t, cancel, done)
	if _, err := testSocketClient(t, config).OpenSSH(context.Background()); err == nil {
		t.Fatal("a worker without the count answered")
	}
}

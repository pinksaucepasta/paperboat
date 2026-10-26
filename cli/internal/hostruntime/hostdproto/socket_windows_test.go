//go:build windows

package hostdproto

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"golang.org/x/sys/windows"
)

type windowsWorkerControlHandlerFunc func(context.Context, WorkerControlRequest) (WorkerControlResponse, error)

func (handler windowsWorkerControlHandlerFunc) HandleWorkerControl(ctx context.Context, request WorkerControlRequest) (WorkerControlResponse, error) {
	return handler(ctx, request)
}

func TestWindowsNamedPipeReportsOperationalFailureAndRecovers(t *testing.T) {
	const privateDetail = "private worker path C:\\Users\\pujan\\AppData\\Local\\paperboat\\worker"
	failure := fmt.Errorf("%s: %w", privateDetail, syscall.EIO)
	config := windowsNativeSocketConfig(t)
	config.WorkerControl = windowsWorkerControlHandlerFunc(func(context.Context, WorkerControlRequest) (WorkerControlResponse, error) {
		return WorkerControlResponse{Completed: true}, failure
	})

	faults := make(chan errorreport.Fault, 4)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	running := startWindowsNativeServer(t, config)

	if _, err := running.client.Request(context.Background(), WorkerControlRequest{Operation: "stop_active"}); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("operational worker failure was not projected to the fixed protocol error: %v", err)
	}
	var fault errorreport.Fault
	select {
	case fault = <-faults:
	case <-time.After(3 * time.Second):
		t.Fatal("authenticated operational failure was not observed")
	}
	if fault.Component != "paperboat-daemon" || fault.Operation != "machine_control" || fault.Stage != "control_request" || fault.Code != "control_request_failed" || fault.Cause != "system_error" || fault.Errno != int(syscall.EIO) || !supportref.Valid(fault.SupportReference) {
		t.Fatalf("fault lost safe classification or request reference: %+v", fault)
	}
	if strings.Contains(fmt.Sprintf("%+v", fault), privateDetail) {
		t.Fatal("fault exposed the worker's private path")
	}
	if _, err := running.client.Active(context.Background()); err != nil {
		t.Fatalf("healthy native request did not recover after operational failure: %v", err)
	}
	select {
	case duplicate := <-faults:
		t.Fatalf("request failure was observed more than once: %+v", duplicate)
	default:
	}
	if err := running.stop(); err != nil {
		t.Fatalf("named-pipe server did not stop cleanly: %v", err)
	}
}

func TestWindowsNamedPipeClientCancellationAndServerJoinReleaseEndpoint(t *testing.T) {
	started := make(chan struct{})
	config := windowsNativeSocketConfig(t)
	config.WorkerControl = windowsWorkerControlHandlerFunc(func(ctx context.Context, _ WorkerControlRequest) (WorkerControlResponse, error) {
		close(started)
		<-ctx.Done()
		return WorkerControlResponse{}, ctx.Err()
	})
	faults := make(chan errorreport.Fault, 2)
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restore()
	running := startWindowsNativeServer(t, config)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	requestDone := make(chan error, 1)
	go func() {
		_, err := running.client.Request(requestCtx, WorkerControlRequest{Operation: "stop_active"})
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("native named-pipe request did not reach its handler")
	}
	cancelRequest()
	select {
	case err := <-requestDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled native client request error=%v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation did not interrupt the established named-pipe request")
	}

	if err := running.stop(); err != nil {
		t.Fatalf("server cancellation did not join the active request cleanly: %v", err)
	}
	var cancellationFault *errorreport.Fault
	select {
	case fault := <-faults:
		cancellationFault = &fault
	default:
	}

	config.WorkerControl = nil
	restarted := startWindowsNativeServer(t, config)
	if _, err := restarted.client.Active(context.Background()); err != nil {
		t.Fatalf("named-pipe endpoint was not released for a replacement server: %v", err)
	}
	if err := restarted.stop(); err != nil {
		t.Fatalf("replacement named-pipe server did not stop cleanly: %v", err)
	}
	if cancellationFault != nil {
		t.Fatalf("ordinary client/server cancellation was reported as an operational failure: %+v", *cancellationFault)
	}
}

type windowsNativeServer struct {
	client *Client
	cancel context.CancelFunc
	done   <-chan error

	stopOnce sync.Once
	stopErr  error
}

func startWindowsNativeServer(t *testing.T, config SocketConfig) *windowsNativeServer {
	t.Helper()
	server, err := NewServer(config)
	if err != nil {
		t.Fatalf("construct native named-pipe server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	client, err := NewClient(config.SocketPath, config.Token, 3*time.Second)
	if err != nil {
		cancel()
		t.Fatalf("construct native named-pipe client: %v", err)
	}
	running := &windowsNativeServer{client: client, cancel: cancel, done: done}
	t.Cleanup(func() {
		if err := running.stop(); err != nil {
			t.Errorf("stop native named-pipe server: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		requestCtx, requestCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, requestErr := client.Active(requestCtx)
		requestCancel()
		if requestErr == nil {
			return running
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("native named-pipe server did not become ready: %v", <-done)
	return nil
}

func (running *windowsNativeServer) stop() error {
	running.stopOnce.Do(func() {
		running.cancel()
		select {
		case running.stopErr = <-running.done:
		case <-time.After(5 * time.Second):
			running.stopErr = errors.New("native named-pipe server did not stop")
		}
	})
	return running.stopErr
}

func windowsNativeSocketConfig(t *testing.T) SocketConfig {
	t.Helper()
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatalf("open current process token: %v", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatalf("get current user SID: %v", err)
	}
	sid := user.User.Sid
	dir := t.TempDir()
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid.String() + ")"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatalf("parse protected state-directory ACL: %v", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read protected state-directory ACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		sid, nil, dacl, nil); err != nil {
		t.Fatalf("protect temporary state directory: %v", err)
	}
	return SocketConfig{
		SocketPath:     fmt.Sprintf(`\\.\pipe\PaperboatHostdTest-%d-%d`, os.Getpid(), time.Now().UnixNano()),
		StatePath:      filepath.Join(dir, "fence.json"),
		SID:            sid.String(),
		Token:          []byte("0123456789abcdef0123456789abcdef"),
		APIMin:         1,
		APIMax:         1,
		RequestTimeout: 3 * time.Second,
	}
}

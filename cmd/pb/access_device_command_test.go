package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type accessDeviceTestRuntime struct {
	mu      sync.Mutex
	dials   int
	closed  bool
	machine string
	port    int
	dial    func(context.Context) (net.Conn, error)
}

func (r *accessDeviceTestRuntime) DialDevice(ctx context.Context, machine string, port int) (net.Conn, error) {
	r.mu.Lock()
	r.dials++
	r.machine, r.port = machine, port
	r.mu.Unlock()
	return r.dial(ctx)
}
func (r *accessDeviceTestRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

type accessDeviceOutput struct{ line chan string }

func (w accessDeviceOutput) Write(p []byte) (int, error) {
	select {
	case w.line <- string(p):
	default:
	}
	return len(p), nil
}

func withAccessDeviceRuntime(t *testing.T, runtime accessDeviceRuntime, machineID string) {
	t.Helper()
	original := accessDeviceRuntimeForCommand
	accessDeviceRuntimeForCommand = func(*cobra.Command, string) (accessDeviceRuntime, string, error) { return runtime, machineID, nil }
	t.Cleanup(func() { accessDeviceRuntimeForCommand = original })
}

func TestAccessDeviceRejectsUnsafeListenerAndPortBeforeAuthentication(t *testing.T) {
	called := false
	original := accessDeviceRuntimeForCommand
	accessDeviceRuntimeForCommand = func(*cobra.Command, string) (accessDeviceRuntime, string, error) {
		called = true
		return nil, "", errors.New("unexpected")
	}
	t.Cleanup(func() { accessDeviceRuntimeForCommand = original })
	for _, args := range [][]string{{"machine", "--port", "0"}, {"machine", "--port", "70000"}, {"machine", "--port", "443", "--listen", "0.0.0.0:0"}, {"machine", "--port", "443", "--listen", "localhost:0"}} {
		command := accessDeviceCobraCommandV1()
		command.SetArgs(args)
		if err := command.Execute(); !errors.Is(err, errAccessDeviceInvalid) {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	if called {
		t.Fatal("invalid input reached authentication runtime")
	}
}

func TestAccessDeviceForwardsFreshAuthorizedFlowsAndHalfClose(t *testing.T) {
	origin, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		for {
			connection, acceptErr := origin.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				request, _ := io.ReadAll(c)
				if len(request) > 0 {
					_, _ = c.Write([]byte("reply:" + string(request)))
				}
			}(connection)
		}
	}()
	runtime := &accessDeviceTestRuntime{dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", origin.Addr().String())
	}}
	withAccessDeviceRuntime(t, runtime, "machine_01")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 1)
	command := accessDeviceCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(accessDeviceOutput{line: lines})
	command.SetArgs([]string{"office", "--port", "5432", "--listen", "127.0.0.1:0"})
	result := make(chan error, 1)
	go func() { result <- command.Execute() }()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not become ready")
	}
	parts := strings.Split(strings.TrimSpace(line), " ")
	endpoint := parts[len(parts)-1]
	local, err := net.DialTimeout("tcp4", endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(local, "hello"); err != nil {
		t.Fatal(err)
	}
	if err = local.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(local)
	_ = local.Close()
	if err != nil || string(reply) != "reply:hello" {
		t.Fatalf("reply=%q err=%v", reply, err)
	}
	cancel()
	select {
	case err = <-result:
		if err != nil {
			t.Fatalf("command exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command ignored cancellation")
	}
	runtime.mu.Lock()
	if runtime.dials < 2 || runtime.machine != "machine_01" || runtime.port != 5432 || !runtime.closed {
		t.Fatalf("runtime dials=%d machine=%q port=%d closed=%v", runtime.dials, runtime.machine, runtime.port, runtime.closed)
	}
	runtime.mu.Unlock()
	_ = origin.Close()
	<-serveDone
}

func TestAccessDeviceRuntimeErrorIsActionableAndDoesNotListen(t *testing.T) {
	original := accessDeviceRuntimeForCommand
	accessDeviceRuntimeForCommand = func(*cobra.Command, string) (accessDeviceRuntime, string, error) {
		return nil, "", fmt.Errorf("Paperboat sign-in credentials are unavailable; run the enrollment command from the Paperboat dashboard, then retry")
	}
	t.Cleanup(func() { accessDeviceRuntimeForCommand = original })
	command := accessDeviceCobraCommandV1()
	command.SetArgs([]string{"office", "--port", "443"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "enrollment command from the Paperboat dashboard") {
		t.Fatalf("error=%v", err)
	}
}

func TestAccessDeviceIsRegisteredExactlyOnce(t *testing.T) {
	root := newRootCommand()
	access, _, err := root.Find([]string{"access"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, child := range access.Commands() {
		if child.Name() == "device" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("access device count=%d", count)
	}
}

func TestAccessDeviceJSONOutputIsBoundedPublicState(t *testing.T) {
	runtime := &accessDeviceTestRuntime{dial: func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go server.Close()
		return client, nil
	}}
	withAccessDeviceRuntime(t, runtime, "machine_01")
	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 1)
	command := accessDeviceCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(accessDeviceOutput{line: lines})
	command.SetArgs([]string{"office", "--port", "443", "--json"})
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("JSON output not written")
	}
	var result accessDeviceResult
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		t.Fatalf("output=%q: %v", line, err)
	}
	if result.Schema != "paperboat.device-access/v1" || result.Kind != "device_access" || result.MachineID != "machine_01" || result.RemotePort != 443 || !strings.HasPrefix(result.ListenAddress, "127.0.0.1:") {
		t.Fatalf("result=%#v", result)
	}
	for _, forbidden := range []string{"token", "credential", "grant", "private_key"} {
		if strings.Contains(strings.ToLower(line), forbidden) {
			t.Fatalf("unsafe output=%s", line)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("JSON command ignored cancellation")
	}
}

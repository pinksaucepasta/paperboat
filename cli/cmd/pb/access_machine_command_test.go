package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	daemonpb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type accessMachineTestRuntime struct {
	mu      sync.Mutex
	dials   int
	closed  bool
	machine string
	port    int
	dial    func(context.Context) (net.Conn, error)
}

func (r *accessMachineTestRuntime) DialMachine(ctx context.Context, machine string, port int) (net.Conn, error) {
	r.mu.Lock()
	r.dials++
	r.machine, r.port = machine, port
	r.mu.Unlock()
	return r.dial(ctx)
}
func (r *accessMachineTestRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

type accessMachineOutput struct{ line chan string }

func (w accessMachineOutput) Write(p []byte) (int, error) {
	select {
	case w.line <- string(p):
	default:
	}
	return len(p), nil
}

func withAccessMachineRuntime(t *testing.T, runtime accessMachineRuntime, machineID string) {
	t.Helper()
	original := accessMachineRuntimeForCommand
	accessMachineRuntimeForCommand = func(*cobra.Command, string) (accessMachineRuntime, string, error) { return runtime, machineID, nil }
	t.Cleanup(func() { accessMachineRuntimeForCommand = original })
}

func TestAccessMachineRejectsUnsafeListenerAndPortBeforeAuthentication(t *testing.T) {
	called := false
	original := accessMachineRuntimeForCommand
	accessMachineRuntimeForCommand = func(*cobra.Command, string) (accessMachineRuntime, string, error) {
		called = true
		return nil, "", errors.New("unexpected")
	}
	t.Cleanup(func() { accessMachineRuntimeForCommand = original })
	for _, args := range [][]string{{"machine", "--port", "0"}, {"machine", "--port", "70000"}, {"machine", "--port", "443", "--listen", "0.0.0.0:0"}, {"machine", "--port", "443", "--listen", "localhost:0"}} {
		command := accessMachineCobraCommandV1()
		command.SetArgs(args)
		if err := command.Execute(); !errors.Is(err, errAccessMachineInvalid) {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	if called {
		t.Fatal("invalid input reached authentication runtime")
	}
}

func TestAccessMachineForwardsFreshAuthorizedFlowsAndHalfClose(t *testing.T) {
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
	runtime := &accessMachineTestRuntime{dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", origin.Addr().String())
	}}
	withAccessMachineRuntime(t, runtime, "machine_01")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 1)
	command := accessMachineCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(accessMachineOutput{line: lines})
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

func TestAccessMachineRuntimeErrorIsActionableAndDoesNotListen(t *testing.T) {
	original := accessMachineRuntimeForCommand
	accessMachineRuntimeForCommand = func(*cobra.Command, string) (accessMachineRuntime, string, error) {
		return nil, "", fmt.Errorf("Paperboat sign-in credentials are unavailable; run the enrollment command from the Paperboat dashboard, then retry")
	}
	t.Cleanup(func() { accessMachineRuntimeForCommand = original })
	command := accessMachineCobraCommandV1()
	command.SetArgs([]string{"office", "--port", "443"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "enrollment command from the Paperboat dashboard") {
		t.Fatalf("error=%v", err)
	}
}

func TestAccessMachineIsRegisteredExactlyOnce(t *testing.T) {
	root := newRootCommand()
	access, _, err := root.Find([]string{"access"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, child := range access.Commands() {
		if child.Name() == "machine" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("access machine count=%d", count)
	}
}

func TestAccessMachineJSONOutputIsBoundedPublicState(t *testing.T) {
	runtime := &accessMachineTestRuntime{dial: func(context.Context) (net.Conn, error) {
		client, server := net.Pipe()
		go server.Close()
		return client, nil
	}}
	withAccessMachineRuntime(t, runtime, "machine_01")
	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 1)
	command := accessMachineCobraCommandV1()
	command.SetContext(ctx)
	command.SetOut(accessMachineOutput{line: lines})
	command.SetArgs([]string{"office", "--port", "443", "--json"})
	done := make(chan error, 1)
	go func() { done <- command.Execute() }()
	var line string
	select {
	case line = <-lines:
	case <-time.After(5 * time.Second):
		t.Fatal("JSON output not written")
	}
	var result accessMachineResult
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		t.Fatalf("output=%q: %v", line, err)
	}
	if result.Schema != "paperboat.machine-access/v1" || result.Kind != "machine_access" || result.MachineID != "machine_01" || result.RemotePort != 443 || !strings.HasPrefix(result.ListenAddress, "127.0.0.1:") {
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

type fakeMachineLocator struct {
	mu      sync.Mutex
	calls   int
	closed  bool
	answers []func() (*daemonpb.MachineAddress, error)
}

func (l *fakeMachineLocator) ResolveMachine(context.Context, string) (*daemonpb.MachineAddress, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	answer := l.answers[min(l.calls, len(l.answers)-1)]
	l.calls++
	return answer()
}

func (l *fakeMachineLocator) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	return nil
}

func machineAnswer(ports ...int32) func() (*daemonpb.MachineAddress, error) {
	return func() (*daemonpb.MachineAddress, error) {
		return &daemonpb.MachineAddress{MachineId: "machine_target", AssignedIp: "127.100.0.9", ForwardedPorts: ports}, nil
	}
}

func TestMachineServiceWaitReturnsOncetheDaemonPublishesTheExactPort(t *testing.T) {
	locator := &fakeMachineLocator{answers: []func() (*daemonpb.MachineAddress, error){machineAnswer(4322), machineAnswer(4322, 4321)}}
	if err := waitForMachineService(t.Context(), locator, "machine_target", 4321); err != nil || locator.calls != 2 {
		t.Fatalf("access returned before the exact port was published: calls=%d err=%v", locator.calls, err)
	}
}

func TestMachineServiceWaitStopsOnUnknownMachineOrCancellation(t *testing.T) {
	unknown := &fakeMachineLocator{answers: []func() (*daemonpb.MachineAddress, error){
		func() (*daemonpb.MachineAddress, error) {
			return nil, status.Error(codes.NotFound, "machine not found")
		}}}
	if err := waitForMachineService(t.Context(), unknown, "machine_target", 4321); !errors.Is(err, resolver.ErrMachineNotFound) || unknown.calls != 1 {
		t.Fatalf("unknown machine: calls=%d err=%v", unknown.calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	locator := &fakeMachineLocator{answers: []func() (*daemonpb.MachineAddress, error){func() (*daemonpb.MachineAddress, error) {
		cancel()
		return &daemonpb.MachineAddress{}, nil
	}}}
	if err := waitForMachineService(ctx, locator, "machine_target", 4321); !errors.Is(err, context.Canceled) || locator.calls != 1 {
		t.Fatalf("cancellation: calls=%d err=%v", locator.calls, err)
	}
}

func TestDaemonAccessRuntimeDialsTheAssignedAddressOfTheExactPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	accepted := make(chan struct{})
	go func() {
		if connection, acceptErr := listener.Accept(); acceptErr == nil {
			connection.Close()
		}
		close(accepted)
	}()
	locator := &fakeMachineLocator{answers: []func() (*daemonpb.MachineAddress, error){func() (*daemonpb.MachineAddress, error) {
		return &daemonpb.MachineAddress{MachineId: "machine_target", AssignedIp: "127.0.0.1", ForwardedPorts: []int32{int32(port)}}, nil
	}}}
	runtime := &daemonAccessMachineRuntime{locator: locator}
	connection, err := runtime.DialMachine(t.Context(), "machine_target", port)
	if err != nil {
		t.Fatal(err)
	}
	connection.Close()
	<-accepted
	if err := runtime.Close(); err != nil || !locator.closed {
		t.Fatalf("daemon connection was not released: closed=%v err=%v", locator.closed, err)
	}
}

type fakeTeamAccess struct {
	workspace string
	team      api.Team
	err       error
}

func (f fakeTeamAccess) Workspace() string { return f.workspace }
func (f fakeTeamAccess) GetTeam(context.Context, string) (api.Team, error) {
	return f.team, f.err
}

func TestTeamMemberIsToldWhyASharedMachinePortIsNotOffered(t *testing.T) {
	cause := errors.New("wait for machine service on port 80")
	granted := api.UserMachine{Permissions: []string{"terminal", "private_access"}}
	for _, test := range []struct {
		name    string
		client  fakeTeamAccess
		machine api.UserMachine
		reason  commandRejectionReason
	}{
		{"no permission", fakeTeamAccess{workspace: "team-a", team: api.Team{AllowPrivateAccess: true}}, api.UserMachine{Permissions: []string{"terminal"}}, commandRejectPrivateAccessNoPermission},
		{"team setting off", fakeTeamAccess{workspace: "team-a"}, granted, commandRejectPrivateAccessTeamOff},
	} {
		var rejection commandRejection
		err := explainTeamPrivateAccess(context.Background(), test.client, test.machine, cause)
		if !errors.As(err, &rejection) || rejection.reason != test.reason {
			t.Fatalf("%s: error = %v, want rejection %v", test.name, err, test.reason)
		}
	}
	// A personal machine, or an allowed team, keeps the original cause.
	for _, client := range []fakeTeamAccess{{workspace: "personal"}, {workspace: "team-a", team: api.Team{AllowPrivateAccess: true}}} {
		if err := explainTeamPrivateAccess(context.Background(), client, granted, cause); err != cause {
			t.Fatalf("error = %v, want the original cause for %+v", err, client)
		}
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
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

func TestProductionAccessWaitsForExactServicePublication(t *testing.T) {
	var catalogs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/machines":
			fmt.Fprint(w, `{"data":{"items":[{"id":"machine_target","alias":"target"}],"pagination":{"limit":200,"offset":0,"total":1,"next_offset":null}}}`)
		case "/v1/machine-services":
			count := catalogs.Add(1)
			port := 4322
			if count > 1 {
				port = 4321
			}
			fmt.Fprintf(w, `{"data":{"items":[{"machine_id":"machine_target","installation_generation":1,"expires_at":%q,"services":[{"port":%d,"address_family":"ipv4"}]}]}}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339), port)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	writeTestProfile(t, directory, path, server.URL)
	command := accessMachineCobraCommandV1()
	command.Flags().String("config", path, "")
	command.Flags().String("server", "", "")
	if err := command.Flags().Set("port", "4321"); err != nil {
		t.Fatal(err)
	}
	command.SetContext(t.Context())
	runtime, machine, err := newProductionAccessMachineRuntime(command, "target")
	if runtime != nil {
		defer runtime.Close()
	}
	if err != nil || machine != "machine_target" || catalogs.Load() < 2 {
		t.Fatalf("access returned before the exact port was published: catalogs=%d machine=%q err=%v", catalogs.Load(), machine, err)
	}
}

func TestMachineServicePublicationWaitStopsOnDenialOrCancellation(t *testing.T) {
	for _, denied := range []bool{true, false} {
		t.Run(fmt.Sprint("denied=", denied), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if denied {
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"error":{"code":"forbidden"}}`)
					return
				}
				fmt.Fprint(w, `{"data":{"items":[]}}`)
				cancel()
			}))
			defer server.Close()
			client := api.New(server.URL, config.Credential{}, server.Client())
			err := waitForMachineService(ctx, client, "machine_target", 4321)
			var failure *api.APIError
			if calls.Load() != 1 || denied && (!errors.As(err, &failure) || failure.Status != http.StatusForbidden) || !denied && !errors.Is(err, context.Canceled) {
				t.Fatalf("publication wait did not stop at its authority/cancellation boundary: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

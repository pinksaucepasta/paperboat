//go:build darwin || linux

package availability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type tokenStub struct{}

func (tokenStub) Token(context.Context) (string, error) { return "identity", nil }

type proofStub struct{ body []byte }

func (p *proofStub) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	if operationID != "operation" || method != http.MethodPost || path != "/v1/helper-runtime-policies/resolve" {
		return nil, errors.New("wrong proof binding")
	}
	p.body = append([]byte(nil), body...)
	return []byte("proof"), nil
}

func TestResolverUsesExactProofAndStrictResponse(t *testing.T) {
	proofs := &proofStub{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer identity" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			t.Error("missing helper authentication")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Resolution{Schema: PolicySchemaV1, UserMachineID: "um_1", Mode: "keep_awake", Version: 0}})
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	resolver, err := NewResolver(server.URL+"/v1/helper-runtime-policies/resolve", tokenStub{}, proofs, func() (string, error) { return "operation", nil }, client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := resolver.Resolve(context.Background())
	if err != nil || result.Version != 0 || result.Mode != "keep_awake" {
		t.Fatalf("resolution=%+v err=%v", result, err)
	}
	if !bytes.Equal(proofs.body, []byte("{}")) {
		t.Fatalf("proof body=%q", proofs.body)
	}
}

func TestResolverPreservesTypedSafeStatusAndDecodeCauses(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		body        string
		wantStatus  int
		wantInvalid bool
	}{
		{name: "upstream status", status: http.StatusServiceUnavailable, body: "private-response-value", wantStatus: http.StatusServiceUnavailable},
		{name: "malformed response", status: http.StatusOK, body: `{"data":{"schema":"private-response-value`, wantInvalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := server.Client()
			client.Timeout = time.Second
			resolver, err := NewResolver(server.URL+"/v1/helper-runtime-policies/resolve", tokenStub{}, &proofStub{}, func() (string, error) { return "operation", nil }, client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = resolver.Resolve(context.Background())
			if err == nil || strings.Contains(err.Error(), "private-response-value") {
				t.Fatalf("unsafe or missing resolver error: %v", err)
			}
			var diagnostic interface {
				DiagnosticStage() string
				DiagnosticCode() string
				DiagnosticStatus() int
			}
			if !errors.As(err, &diagnostic) || diagnostic.DiagnosticStage() != controlRequestStage || diagnostic.DiagnosticCode() != controlRequestCode || diagnostic.DiagnosticStatus() != test.wantStatus {
				t.Fatalf("diagnostic classification=%v", err)
			}
			if test.wantInvalid && !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed response lost invalid-contract sentinel: %v", err)
			}
			if test.wantInvalid {
				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("malformed response lost decode cause: %v", err)
				}
			}
		})
	}
}

func TestHostClientAppliesVersionZeroAndRejectsMismatchedResponse(t *testing.T) {
	for name, response := range map[string]hostservice.Response{
		"valid":                            {Schema: hostservice.ProtocolV1, Status: "applied", DesiredMode: "allow_sleep", DesiredVersion: 0, ObservedMode: "allow_sleep", ObservedVersion: 0, ObservedAt: time.Now().UTC(), HostServiceVersion: "test", Scope: "system", UpdateHealth: "healthy"},
		"valid-without-update-diagnostics": {Schema: hostservice.ProtocolV1, Status: "applied", DesiredMode: "allow_sleep", DesiredVersion: 0, ObservedMode: "allow_sleep", ObservedVersion: 0, ObservedAt: time.Now().UTC(), HostServiceVersion: "test", Scope: "system", UpdateHealth: "unknown"},
		"mismatch":                         {Schema: hostservice.ProtocolV1, Status: "applied", DesiredMode: "keep_awake", DesiredVersion: 1, ObservedMode: "keep_awake", ObservedVersion: 1, ObservedAt: time.Now().UTC(), HostServiceVersion: "test", Scope: "system"},
	} {
		t.Run(name, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "pba-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			socket := filepath.Join(root, "host.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				connection, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				defer connection.Close()
				var request hostservice.Request
				_ = json.NewDecoder(connection).Decode(&request)
				_ = json.NewEncoder(connection).Encode(response)
			}()
			client, err := NewHostClient(socket, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			observation, err := client.Apply(context.Background(), Resolution{Schema: PolicySchemaV1, UserMachineID: "um_1", Mode: "allow_sleep", Version: 0})
			if strings.HasPrefix(name, "valid") && (err != nil || observation.Version != 0) {
				t.Fatalf("observation=%+v err=%v", observation, err)
			}
			if name == "mismatch" && !errors.Is(err, ErrInvalid) {
				t.Fatalf("mismatch error=%v", err)
			}
		})
	}
}

type flakyResolver struct {
	mu    sync.Mutex
	calls int
}

func (r *flakyResolver) Resolve(context.Context) (Resolution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == 1 {
		return Resolution{}, errors.New("offline")
	}
	return Resolution{Schema: PolicySchemaV1, UserMachineID: "um_1", Mode: "keep_awake", Version: 2}, nil
}

type hostStub struct{ applied chan Resolution }

func (h hostStub) Apply(_ context.Context, resolution Resolution) (Observation, error) {
	h.applied <- resolution
	return Observation{Schema: PolicySchemaV1, Mode: resolution.Mode, Version: resolution.Version, Status: "applied", ObservedAt: time.Now().UTC(), HostServiceVersion: "test", HostServiceScope: "system", UpdateHealth: "healthy"}, nil
}

type immediateResolver struct{}

func (immediateResolver) Resolve(context.Context) (Resolution, error) {
	return Resolution{Schema: PolicySchemaV1, UserMachineID: "um_1", Mode: "keep_awake", Version: 3}, nil
}

func TestServicePublishesInitialObservationBeforeStartReturns(t *testing.T) {
	host := hostStub{applied: make(chan Resolution, 2)}
	service, err := NewService(immediateResolver{}, host, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	observation := service.Observation()
	if observation == nil || observation.Version != 3 || observation.Status != "applied" {
		t.Fatalf("initial observation=%+v", observation)
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestServiceStartsOfflineAndEventuallyPublishesObservation(t *testing.T) {
	resolver := &flakyResolver{}
	host := hostStub{applied: make(chan Resolution, 1)}
	service, err := NewService(resolver, host, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := service.Start(ctx); err != nil {
		t.Fatalf("offline start: %v", err)
	}
	select {
	case policy := <-host.applied:
		if policy.Version != 2 {
			t.Fatalf("policy=%+v", policy)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("policy was not retried")
	}
	deadline := time.Now().Add(time.Second)
	for {
		if observation := service.Observation(); observation != nil {
			if observation.Version != 2 {
				t.Fatalf("observation=%+v", observation)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observation=%+v", service.Observation())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRejectsAlreadyCanceledStart(t *testing.T) {
	service, err := NewService(immediateResolver{}, hostStub{applied: make(chan Resolution, 1)}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error=%v", err)
	}
	if observation := service.Observation(); observation != nil {
		t.Fatalf("canceled start installed work: %+v", observation)
	}
}

func TestServiceObservesLocalGatewayOutageOnceAndRecovers(t *testing.T) {
	type observedFault struct {
		ctx   context.Context
		fault errorreport.Fault
	}
	var mu sync.Mutex
	var faults []observedFault
	restoreObserver := errorreport.InstallFaultObserver(func(ctx context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, observedFault{ctx: ctx, fault: fault})
		mu.Unlock()
	})
	defer restoreObserver()

	socket := filepath.Join(t.TempDir(), "host.sock")
	host, err := NewHostClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(immediateResolver{}, host, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reference := supportref.New()
	ctx, cancel := context.WithCancel(supportref.WithContext(context.Background(), reference))
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		observed := len(faults) > 0
		mu.Unlock()
		if observed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local gateway outage was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		var request hostservice.Request
		if json.NewDecoder(connection).Decode(&request) != nil || request.Operation != "apply_availability" {
			return
		}
		_ = json.NewEncoder(connection).Encode(hostservice.Response{
			Schema: hostservice.ProtocolV1, Status: "applied", DesiredMode: "keep_awake", DesiredVersion: 3,
			ObservedMode: "keep_awake", ObservedVersion: 3, ObservedAt: time.Now().UTC(),
			HostServiceVersion: "test", Scope: "system", UpdateHealth: "healthy",
		})
	}()

	deadline = time.Now().Add(4 * time.Second)
	for {
		if observation := service.Observation(); observation != nil {
			if observation.Version != 3 || observation.Status != "applied" {
				t.Fatalf("recovered availability observation=%+v", observation)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("availability did not recover after local gateway returned")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("host service did not complete the recovery request")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 1 {
		t.Fatalf("repeated local outage observations=%d, want one", len(faults))
	}
	fault := faults[0].fault
	if fault.Stage != localGatewayStage || fault.Code != localGatewayCode || fault.Cause != "not_found" || fault.SupportReference != reference || supportref.FromContext(faults[0].ctx) != reference {
		t.Fatalf("local gateway fault=%+v", fault)
	}
}

func TestServiceDoesNotDuplicateOwnedControlAttempt(t *testing.T) {
	type observedFault struct {
		fault errorreport.Fault
	}
	var mu sync.Mutex
	var faults []observedFault
	restoreObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, observedFault{fault: fault})
		mu.Unlock()
	})
	defer restoreObserver()

	var requests int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("private-response-value"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Resolution{Schema: PolicySchemaV1, UserMachineID: "um_1", Mode: "keep_awake", Version: 3}})
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = time.Second
	client.Transport = errorreport.TransportOperation(client.Transport, server.URL, "runtime_observation")
	resolver, err := NewResolver(server.URL+"/v1/helper-runtime-policies/resolve", tokenStub{}, &proofStub{}, func() (string, error) { return "operation", nil }, client)
	if err != nil {
		t.Fatal(err)
	}
	host := hostStub{applied: make(chan Resolution, 1)}
	service, err := NewService(resolver, host, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-host.applied:
	case <-time.After(time.Second):
		t.Fatal("control request did not recover")
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
	defer shutdownCancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(faults) != 1 {
		t.Fatalf("owned control attempt was reported %d times, want once", len(faults))
	}
	fault := faults[0].fault
	if fault.Stage != controlRequestStage || fault.Code != controlRequestCode || fault.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("owned control attempt fault=%+v", fault)
	}
	if strings.Contains(faults[0].fault.Cause, "private-response-value") {
		t.Fatalf("response body escaped in fault: %+v", fault)
	}
}

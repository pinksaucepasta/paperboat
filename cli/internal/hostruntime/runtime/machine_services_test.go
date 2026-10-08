//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

func TestMachineServiceAnnouncementPolicyAndWithdrawal(t *testing.T) {
	fail := false
	s := &runtimeObservationSender{installationGeneration: 7, lazyBootID: "boot_a", lazyStartedAt: time.Now(), machineServicesDiscover: func(context.Context) ([]machineservices.Service, error) {
		if fail {
			return nil, errors.New("socket table unavailable")
		}
		out := []machineservices.Service{{Port: 22, Loopback: "127.0.0.1"}}
		for p := 1100; p >= 1024; p-- {
			out = append(out, machineservices.Service{Port: uint16(p), Loopback: "127.0.0.1"})
		}
		return out, nil
	}}
	first := s.machineServicesObservation(context.Background())
	if first.Generation != 1 || len(first.Services) != 0 {
		t.Fatalf("unconfigured discovery exposed ports: %+v", first)
	}
	s.applyMachineServicesPolicy([]byte(`{"data":{"machine_services_policy":{"generation":1,"automatic_discovery_enabled":true}}}`))
	active := s.machineServicesObservation(context.Background())
	if active.Generation != 2 || len(active.Services) != 64 || active.Services[0].Port != 1024 || active.Services[63].Port != 1087 {
		t.Fatalf("wrong bounded snapshot: %+v", active)
	}
	active.Services[0].Port = 9
	if got := s.machineServicesObservation(context.Background()); got.Generation != 2 || got.Services[0].Port != 1024 {
		t.Fatal("snapshot alias or unstable generation", got)
	}
	fail = true
	withdrawn := s.machineServicesObservation(context.Background())
	if withdrawn.Generation != 3 || len(withdrawn.Services) != 0 {
		t.Fatal("failed discovery retained ports", withdrawn)
	}
	fail = false
	s.applyMachineServicesPolicy([]byte(`{"data":{"machine_services_policy":{"generation":2,"explicit_ports":[{"port":22,"address_family":"ipv4"}]}}}`))
	if got := s.machineServicesObservation(context.Background()); !reflect.DeepEqual(got.Services, []api.MachineServicePort{{Port: 22, AddressFamily: "ipv4"}}) {
		t.Fatal("explicit policy", got)
	}
	s.applyMachineServicesPolicy([]byte(`{"data":{"machine_services_policy":{"generation":3,"automatic_discovery_enabled":true,"explicit_ports":[{"port":0,"address_family":"ipv4"}]}}}`))
	if got := s.machineServicesObservation(context.Background()); len(got.Services) != 0 {
		t.Fatal("invalid policy did not withdraw", got)
	}
}

func TestMachineServiceCurrentAuthorityExactBinding(t *testing.T) {
	calls := 0
	denied := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/runtime-machine-services/authorize" || r.Header.Get("Authorization") != "Bearer runtime-observation-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			t.Error("missing runtime authentication")
		}
		var body struct {
			UserID   string                `json:"user_id"`
			CLI      string                `json:"cli_client_session_id"`
			Access   string                `json:"access_session_id"`
			Expected nativeprivate.Binding `json:"expected"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.UserID != "user_a" || body.CLI != "cli_a" || body.Access != "access_a" || body.Expected.TargetAddress != "127.0.0.1:3000" || body.Expected.UserID != "" {
			t.Error("wrong exact actor/target body", body)
		}
		if denied {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"expires_at": time.Now().Add(time.Minute)}})
	}))
	defer server.Close()
	s := &runtimeObservationSender{endpoint: server.URL + "/v1/runtime-observations", machineID: "machine_a", environmentID: "env_a", installationGeneration: 7, lazyBootID: "boot_a", machineServicesGeneration: 3, tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_a", nil }, client: server.Client()}
	b := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "machine_service", ResourceID: "machine_a", OwnerEndpointID: "machine_a", RouteID: "tcp:3000", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:3000", ExpiresAt: time.Now().Add(time.Minute), InstallationGeneration: 7, BootID: "boot_a", PolicyGeneration: 2, AnnouncementGeneration: 3, UserID: "user_a", CLIClientSessionID: "cli_a", AccessSessionID: "access_a"}
	if err := s.validateMachineService(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	denied = true
	if err := s.validateMachineService(context.Background(), b); err == nil {
		t.Fatal("revocation accepted")
	}
	b.AnnouncementGeneration--
	if err := s.validateMachineService(context.Background(), b); err == nil || calls != 2 {
		t.Fatal("stale snapshot reached authority", err, calls)
	}
}

func TestExplicitMachineServiceProbePolicyAndWithdrawal(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-stopped })
	port := listener.Addr().(*net.TCPAddr).Port
	s := &runtimeObservationSender{installationGeneration: 7, lazyBootID: "boot_explicit", lazyStartedAt: time.Now(), machineServicesDiscover: func(context.Context) ([]machineservices.Service, error) {
		return nil, errors.New("socket table unavailable")
	}}
	if got := s.machineServicesObservation(t.Context()); len(got.Services) != 0 {
		t.Fatal("unpublished listener announced")
	}
	policy, _ := json.Marshal(map[string]any{"data": map[string]any{"machine_services_policy": api.MachineServicesPolicy{Generation: 1, ExplicitPorts: []api.MachineServicePort{{Port: port, AddressFamily: "ipv4"}}}}})
	s.applyMachineServicesPolicy(policy)
	active := s.machineServicesObservation(t.Context())
	if !reflect.DeepEqual(active.Services, []api.MachineServicePort{{Port: port, AddressFamily: "ipv4"}}) {
		t.Fatalf("explicit listener without socket ownership: %+v", active)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelled := s.machineServicesObservation(ctx)
	if len(cancelled.Services) != 0 || cancelled.Generation <= active.Generation {
		t.Fatal("cancelled probe retained stale availability", cancelled)
	}
	recovered := s.machineServicesObservation(t.Context())
	if len(recovered.Services) != 1 || recovered.Generation <= cancelled.Generation {
		t.Fatal("explicit service failed to recover", recovered)
	}
	s.applyMachineServicesPolicy([]byte(`{"data":{"machine_services_policy":{"generation":2}}}`))
	withdrawn := s.machineServicesObservation(t.Context())
	if len(withdrawn.Services) != 0 || withdrawn.Generation <= recovered.Generation {
		t.Fatal("removed publication retained availability", withdrawn)
	}
	s.applyMachineServicesPolicy(policy)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.machineServicesObservation(t.Context()); len(got.Services) != 0 {
		t.Fatal("closed explicit service announced", got)
	}
}

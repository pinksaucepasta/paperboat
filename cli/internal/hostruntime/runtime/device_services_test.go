//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/deviceservices"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
)

func TestDeviceServiceAnnouncementPolicyAndWithdrawal(t *testing.T) {
	fail := false
	s := &runtimeObservationSender{setupMode: "host", installationGeneration: 7, lazyBootID: "boot_a", lazyStartedAt: time.Now(), deviceServicesDiscover: func(context.Context) ([]deviceservices.Service, error) {
		if fail {
			return nil, errors.New("socket table unavailable")
		}
		out := []deviceservices.Service{{Port: 22, Loopback: "127.0.0.1"}}
		for p := 1100; p >= 1024; p-- {
			out = append(out, deviceservices.Service{Port: uint16(p), Loopback: "127.0.0.1"})
		}
		return out, nil
	}}
	first := s.deviceServicesObservation(context.Background())
	if first.Generation != 1 || len(first.Services) != 0 {
		t.Fatalf("unconfigured discovery exposed ports: %+v", first)
	}
	s.applyDeviceServicesPolicy([]byte(`{"data":{"device_services_policy":{"generation":1,"automatic_discovery_enabled":true}}}`))
	active := s.deviceServicesObservation(context.Background())
	if active.Generation != 2 || len(active.Services) != 64 || active.Services[0].Port != 1024 || active.Services[63].Port != 1087 {
		t.Fatalf("wrong bounded snapshot: %+v", active)
	}
	active.Services[0].Port = 9
	if got := s.deviceServicesObservation(context.Background()); got.Generation != 2 || got.Services[0].Port != 1024 {
		t.Fatal("snapshot alias or unstable generation", got)
	}
	fail = true
	withdrawn := s.deviceServicesObservation(context.Background())
	if withdrawn.Generation != 3 || len(withdrawn.Services) != 0 {
		t.Fatal("failed discovery retained ports", withdrawn)
	}
	fail = false
	s.applyDeviceServicesPolicy([]byte(`{"data":{"device_services_policy":{"generation":2,"explicit_ports":[{"port":22,"address_family":"ipv4"}]}}}`))
	if got := s.deviceServicesObservation(context.Background()); !reflect.DeepEqual(got.Services, []api.DeviceServicePort{{Port: 22, AddressFamily: "ipv4"}}) {
		t.Fatal("explicit policy", got)
	}
	s.applyDeviceServicesPolicy([]byte(`{"data":{"device_services_policy":{"generation":3,"automatic_discovery_enabled":true,"explicit_ports":[{"port":0,"address_family":"ipv4"}]}}}`))
	if got := s.deviceServicesObservation(context.Background()); len(got.Services) != 0 {
		t.Fatal("invalid policy did not withdraw", got)
	}
}

func TestDeviceServiceCurrentAuthorityExactBinding(t *testing.T) {
	calls := 0
	denied := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/runtime-device-services/authorize" || r.Header.Get("Authorization") != "Bearer runtime-observation-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
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
	s := &runtimeObservationSender{endpoint: server.URL + "/v1/runtime-observations", machineID: "machine_a", environmentID: "env_a", installationGeneration: 7, lazyBootID: "boot_a", deviceServicesGeneration: 3, tokens: livenessObservationTokenSource{}, proofs: livenessObservationProofSource{}, operationID: func() (string, error) { return "operation_a", nil }, client: server.Client()}
	b := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "device_service", ResourceID: "machine_a", OwnerEndpointID: "machine_a", RouteID: "tcp:3000", ResourceGeneration: 1, RouteGeneration: 1, TargetGeneration: 1, Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:3000", ExpiresAt: time.Now().Add(time.Minute), InstallationGeneration: 7, BootID: "boot_a", PolicyGeneration: 2, AnnouncementGeneration: 3, UserID: "user_a", CLIClientSessionID: "cli_a", AccessSessionID: "access_a"}
	if err := s.validateDeviceService(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	denied = true
	if err := s.validateDeviceService(context.Background(), b); err == nil {
		t.Fatal("revocation accepted")
	}
	b.AnnouncementGeneration--
	if err := s.validateDeviceService(context.Background(), b); err == nil || calls != 2 {
		t.Fatal("stale snapshot reached authority", err, calls)
	}
}

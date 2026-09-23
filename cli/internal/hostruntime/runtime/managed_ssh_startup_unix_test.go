//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagedSSHStartsClosedDuringHTTPOutageAndRecovers(t *testing.T) {
	key := managedSSHTestPublicKey(t)
	home := managedSSHRuntimeTestHome(t)
	if _, err := reconcilePlatformAuthorizedKeys(home, uint32(os.Getuid()), []string{key}); err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Paperboat-Machine-Identity") == "" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			w.WriteHeader(401)
			return
		}
		if !available.Load() {
			w.WriteHeader(501)
			return
		}
		var value any
		switch r.URL.Path {
		case "/v1/machines/machine_1/ssh-host-keys":
			if r.Method != http.MethodPut {
				w.WriteHeader(405)
				return
			}
			value = clientapi.ManagedSSHHostKeySet{Type: "host_key_set", Version: 1, SetID: "set_1", MachineID: "machine_1", MachineGeneration: 4, ObservationGeneration: 9, Keys: []string{key}, State: "active", ReconciliationVersion: 1}
		case "/v1/machines/machine_1/ssh-authorized-keys":
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				return
			}
			value = clientapi.ManagedSSHAuthorizedKeys{Type: "authorized_key_set", Version: 1, MachineID: "machine_1", MachineGeneration: 4, Keys: []string{key}}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": value})
	}))
	defer server.Close()
	service := &managedSSHKeyReconciler{client: clientapi.New(server.URL, clientconfig.Credential{}, server.Client()), identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, setID: "set_1", publicKeys: []string{key}, home: home, ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	path := filepath.Join(home, ".ssh", "authorized_keys")
	assertKey := func(want bool) {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(key)) != want {
			t.Fatalf("managed key presence != %t", want)
		}
	}
	assertKey(false)
	available.Store(true)
	deadline := time.Now().Add(time.Second)
	for {
		raw, _ := os.ReadFile(path)
		if bytes.Contains(raw, []byte(key)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authority recovery did not restore managed key")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertKey(false)
}

func TestManagedSSHInitialAuthorityPolicyAndLocalFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"unavailable", &clientapi.APIError{Status: 503}, true}, {"rate_limit", &clientapi.APIError{Status: 429}, true}, {"revoked", &clientapi.APIError{Status: 403}, false}, {"expired", clientapi.ErrUnauthenticated, false}, {"conflict", &clientapi.APIError{Status: 409}, false}, {"bad_response", errors.New("invalid response"), false}, {"cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := managedSSHRuntimeTestHome(t)
			key := managedSSHTestPublicKey(t)
			if _, err := reconcilePlatformAuthorizedKeys(home, uint32(os.Getuid()), []string{key}); err != nil {
				t.Fatal(err)
			}
			service := &managedSSHKeyReconciler{client: &rotatingManagedSSHClient{observeErrors: []error{tc.err}, keys: [][]string{{key}}}, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, setID: "set_1", publicKeys: []string{key}, home: home, ownerUID: uint32(os.Getuid()), interval: time.Hour, timeout: time.Second}
			err := service.Start(t.Context())
			defer service.Shutdown(context.Background())
			if (err == nil) != tc.retry {
				t.Fatalf("startup error=%v retry=%t", err, tc.retry)
			}
			raw, readErr := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if bytes.Contains(raw, []byte(key)) {
				t.Fatal("unavailable or rejected authority retained old key")
			}
		})
	}
	for _, authorityErr := range []error{&clientapi.APIError{Status: 503}, nil} {
		home := managedSSHRuntimeTestHome(t)
		if err := os.Symlink(t.TempDir(), filepath.Join(home, ".ssh")); err != nil {
			t.Fatal(err)
		}
		client := &rotatingManagedSSHClient{observeErrors: []error{authorityErr}, keys: [][]string{{managedSSHTestPublicKey(t)}}}
		service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, setID: "set_1", publicKeys: []string{"ssh-ed25519 AAAA host"}, home: home, ownerUID: uint32(os.Getuid()), interval: time.Hour, timeout: time.Second}
		if err := service.Start(t.Context()); !errors.Is(err, ErrManagedSSHUnavailable) {
			t.Fatalf("unsafe local key path did not fail startup: %v", err)
		}
		if service.cancel != nil {
			t.Fatal("local write/cleanup failure started retry loop")
		}
	}
}

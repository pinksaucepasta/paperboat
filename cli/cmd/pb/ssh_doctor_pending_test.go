package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// A host key outside the active set with a newer set waiting for trust: the doctor names the change and the
// trust-host command, not the generic failure. Other failures, or no pending set, keep their own path.
func TestSSHDoctorNamesAPendingHostKeyChange(t *testing.T) {
	pending := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !pending || r.URL.Query().Get("state") != "pending" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"type":"host_key_set","version":1,"set_id":"keyset_1","machine_id":"machine_1","machine_generation":1,"observation_generation":2,"keys":["ssh-ed25519 AAAA"],"fingerprint":"SHA256:pendingFingerprint","state":"pending","reconciliation_version":3}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	changed := fmt.Errorf("probe: %w", errors.Join(managedssh.ErrSSHAuthentication, managedssh.ErrKnownHosts))

	err := pendingHostKeyFailure(t.Context(), client, machine, changed)
	if err == nil || !errors.Is(err, managedssh.ErrKnownHosts) {
		t.Fatalf("pending change: %v", err)
	}
	message := classifyCLIJSONError(err).Message
	if !strings.Contains(message, "pb ssh trust-host studio --fingerprint SHA256:pendingFingerprint") || strings.Contains(message, "could not finish") {
		t.Fatalf("doctor message = %q", message)
	}
	if pendingHostKeyFailure(t.Context(), client, machine, errors.New("auth refused")) != nil {
		t.Fatal("a failure that is not a host key mismatch was reported as a pending change")
	}
	pending = false
	err = pendingHostKeyFailure(t.Context(), client, machine, changed)
	message = classifyCLIJSONError(err).Message
	if err == nil || strings.Contains(message, "trust-host") || strings.Contains(message, "could not finish") || !strings.Contains(message, "do not connect") {
		t.Fatalf("a mismatch with nothing pending: %v, message %q", err, message)
	}
}

// C066 on Windows: `pb ssh <machine> -- <command>` runs SSH in pb, so a changed host key arrives as the handshake's
// ErrKnownHosts. It gets the trust-host refusal Linux and macOS print after OpenSSH's exit 255.
func TestPbSSHInProcessNamesAPendingHostKeyChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"type":"host_key_set","version":1,"set_id":"keyset_1","machine_id":"machine_1","machine_generation":1,"observation_generation":2,"keys":["ssh-ed25519 AAAA"],"fingerprint":"SHA256:pendingFingerprint","state":"pending","reconciliation_version":3}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	clientFor := func() (*api.Client, error) { return client, nil }
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}

	err := changedSSHHostFailure(t.Context(), machine, fmt.Errorf("ssh: handshake failed: %w", managedssh.ErrKnownHosts), clientFor)
	message := classifyCLIJSONError(err).Message
	if !strings.Contains(message, "pb ssh trust-host studio --fingerprint SHA256:pendingFingerprint") {
		t.Fatalf("message = %q", message)
	}
	if changedSSHHostFailure(t.Context(), machine, managedssh.NativeExitError{Code: 255}, clientFor) != nil {
		t.Fatal("OpenSSH's exit 255 keeps its own explanation")
	}
}

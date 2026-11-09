package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
)

// C066: after the target's host keys change, OpenSSH fails (exit 255) with its own known_hosts advice; pb
// ssh adds the exact pending identity and the trust-host command. Other failures add nothing.
func TestManagedSSHNamesTheTrustHostCommandAfterAHostKeyChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("state") != "pending" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"type":"host_key_set","version":1,"set_id":"keyset_1","machine_id":"machine_1","machine_generation":1,"observation_generation":2,"keys":["ssh-ed25519 AAAA"],"fingerprint":"SHA256:pendingFingerprint","state":"pending","reconciliation_version":3}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"host key changed": {managedssh.NativeExitError{Code: 255}, true},
		"remote exit 1":    {managedssh.NativeExitError{Code: 1}, false},
		"other failure":    {errors.New("dial"), false},
	} {
		var stderr bytes.Buffer
		command := &cobra.Command{}
		command.SetErr(&stderr)
		command.SetContext(t.Context())
		explainChangedSSHHostIdentity(command, client, machine, test.err)
		got := strings.Contains(stderr.String(), "pb ssh trust-host studio --fingerprint SHA256:pendingFingerprint")
		if got != test.want {
			t.Fatalf("%s: stderr %q", name, stderr.String())
		}
	}
}

// F5-10: a session closed by revocation says so, for a signed-out CLI (401) and for a withdrawn grant (403); other
// failures say nothing.
func TestManagedSSHSaysAccessWasRevoked(t *testing.T) {
	status := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"x","message":"x"}}`))
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	for name, test := range map[string]struct {
		status int
		err    error
		want   string
	}{
		"signed out":       {401, managedssh.NativeExitError{Code: 255}, "access to studio was revoked: this sign-in was ended"},
		"grant withdrawn":  {403, managedssh.NativeExitError{Code: 255}, "access to studio was revoked: your account no longer has permission"},
		"no pending keys":  {404, managedssh.NativeExitError{Code: 255}, ""},
		"remote exit code": {401, managedssh.NativeExitError{Code: 1}, ""},
	} {
		status = test.status
		var stderr bytes.Buffer
		command := &cobra.Command{}
		command.SetErr(&stderr)
		command.SetContext(t.Context())
		said := explainRevokedSSHAccess(command, client, machine, test.err)
		if said != (test.want != "") || !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("%s: said=%v stderr %q", name, said, stderr.String())
		}
	}
}

// F5-10: after `pb logout` this computer has no credentials, so no server can be asked; the session the host
// closed is still explained as a revocation.
func TestManagedSSHSaysAccessWasRevokedWhenSignedOut(t *testing.T) {
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	explainSSHFailure(command, machine, managedssh.NativeExitError{Code: 255}, func() (*api.Client, error) {
		return nil, fmt.Errorf("load credential: %w", config.ErrNoCredentials)
	})
	if !strings.Contains(stderr.String(), "access to studio was revoked: this sign-in was ended") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// F5-10: a plain `ssh` through the managed config shows this ProxyCommand's stderr. The host closing the stream
// while OpenSSH still holds it open, after `pb logout`, says the access was revoked.
func TestSSHProxySaysAccessWasRevokedWhenTheHostClosedTheStream(t *testing.T) {
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	explainEndedSSHProxy(command, machine, false, func() (*api.Client, error) {
		return nil, fmt.Errorf("load credential: %w", config.ErrNoCredentials)
	})
	if !strings.Contains(stderr.String(), "access to studio was revoked: this sign-in was ended") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// An ordinary logout (OpenSSH closed its side first) prints nothing and asks nothing, even when signed out.
func TestSSHProxyStaysSilentOnAnOrdinaryClose(t *testing.T) {
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	explainEndedSSHProxy(command, machine, true, func() (*api.Client, error) {
		t.Error("an ordinary close asked for a client")
		return nil, config.ErrNoCredentials
	})
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestEOFTrackerSeesOpenSSHClosingTheInput(t *testing.T) {
	input := &eofTracker{Reader: strings.NewReader("x")}
	if _, err := io.Copy(io.Discard, input); err != nil || !input.sawEOF() {
		t.Fatalf("err = %v, sawEOF = %v", err, input.sawEOF())
	}
	if (&eofTracker{Reader: strings.NewReader("x")}).sawEOF() {
		t.Fatal("EOF reported before the input was read")
	}
}

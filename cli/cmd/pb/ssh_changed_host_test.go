package main

import (
	"bytes"
	"errors"
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

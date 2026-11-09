package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

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

// F5-10: a session closed by revocation says so: a signed-out CLI (401), a machine the account can no longer see (a
// withdrawn grant or share, a removed member: 404 user_machine_not_found; the host-key reads answer 404 for that and for an
// ordinary missing pending set alike, so they cannot be the probe) and a machine still visible without managed_ssh. A
// machine that is still usable, and other failures, say nothing.
func TestManagedSSHSaysAccessWasRevoked(t *testing.T) {
	status, code, body := 0, "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/machines/machine_1" {
			t.Errorf("probe asked %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"x"}}`, code)
	}))
	defer server.Close()
	client := api.New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	visible := func(ownership, permissions string) string {
		return `{"data":{"id":"machine_1","ownership":"` + ownership + `","permissions":[` + permissions + `]}}`
	}
	for name, test := range map[string]struct {
		status int
		code   string
		body   string
		err    error
		want   string
	}{
		"signed out":               {401, "unauthenticated", "", managedssh.NativeExitError{Code: 255}, "access to studio was revoked: this sign-in was ended"},
		"grant or share withdrawn": {404, "user_machine_not_found", "", managedssh.NativeExitError{Code: 255}, "access to studio was revoked: it is no longer shared with you"},
		"forbidden":                {403, "forbidden", "", managedssh.NativeExitError{Code: 255}, "access to studio was revoked: your account no longer has permission"},
		"managed_ssh withdrawn":    {200, "", visible("team", `"exec"`), managedssh.NativeExitError{Code: 255}, "access to studio was revoked: your account no longer has permission"},
		"still granted":            {200, "", visible("team", `"exec","managed_ssh"`), managedssh.NativeExitError{Code: 255}, ""},
		"own machine":              {200, "", visible("personal", ``), managedssh.NativeExitError{Code: 255}, ""},
		"server error":             {500, "internal_error", "", managedssh.NativeExitError{Code: 255}, ""},
		"remote exit code":         {401, "unauthenticated", "", managedssh.NativeExitError{Code: 1}, ""},
	} {
		status, code, body = test.status, test.code, test.body
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

// `pb ssh` prints the message itself after OpenSSH exits and marks its environment, so the proxy inside it is silent.
func TestSSHProxyStaysSilentInsidePbSSH(t *testing.T) {
	t.Setenv(sshMessageOwnerEnv, "1")
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	explainEndedSSHProxy(command, machine, false, func() (*api.Client, error) {
		t.Error("the proxy inside pb ssh asked for a client")
		return nil, config.ErrNoCredentials
	})
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestPbSSHMarksTheEnvironmentOfItsOpenSSH(t *testing.T) {
	if got := pbSSHEnvironment([]string{"A=b"}); !slices.Contains(got, sshMessageOwnerEnv+"=1") || got[0] != "A=b" {
		t.Fatalf("environment = %v", got)
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

// An established stream the host closed (revocation) ends the proxy at once even while OpenSSH holds its input open, with
// no dial retry (those cover only opening the stream), and then says why: the whole path of a plain ssh after `pb logout`.
func TestEstablishedSSHStreamClosedByTheHostEndsTheProxyAndSaysWhy(t *testing.T) {
	proxy, remote := net.Pipe()
	inputReader, inputWriter := io.Pipe()
	defer inputWriter.Close()
	connection := &sshProxyTestConn{Conn: proxy, closeWrite: func() error { return nil }}
	input := &eofTracker{Reader: inputReader}
	done := make(chan error, 1)
	go func() { done <- copySSHProxy(connection, connection, input, io.Discard) }()
	_, _ = remote.Write([]byte("SSH-2.0-banner\r\n"))
	_ = remote.Close() // the host closes the stream: access ended
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the proxy stayed alive after the host closed an established stream")
	}
	var stderr bytes.Buffer
	command := &cobra.Command{}
	command.SetErr(&stderr)
	machine := api.UserMachine{ID: "machine_1", Alias: "studio", InstallationGeneration: 1}
	explainEndedSSHProxy(command, machine, input.sawEOF(), func() (*api.Client, error) {
		return nil, fmt.Errorf("load credential: %w", config.ErrNoCredentials)
	})
	if !strings.Contains(stderr.String(), "access to studio was revoked: this sign-in was ended") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSSHDialNamesAnUnclassifiedNetworkFailure(t *testing.T) {
	err := explainSSHDial("studio", &net.OpError{Op: "read", Net: "unix", Err: errors.New("i/o timeout")})
	if message := sshFailureMessage(err); !strings.Contains(message, "studio") || !strings.Contains(message, "(step: open stream)") || strings.Contains(message, "unreachable") {
		t.Fatalf("message = %q", message)
	}
}

// A completed session closes the stream under the copy loop (OpenSSH exited, taking the proxy's context with it, or
// closed its input): the proxy exits 0 silently. device: a team member's plain `ssh host exit 0` printed "Paperboat is
// unreachable" from the proxy's "use of closed network connection".
func TestSSHProxyTreatsTheCloseOfACompletedSessionAsTheEnd(t *testing.T) {
	closed := &net.OpError{Op: "read", Net: "unix", Err: net.ErrClosed}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := finishSSHProxy(canceled, closed, false); err != nil {
		t.Fatalf("closed stream after the context ended = %v", err)
	}
	if err := finishSSHProxy(t.Context(), closed, true); err != nil {
		t.Fatalf("closed stream after OpenSSH closed its input = %v", err)
	}
	if err := finishSSHProxy(t.Context(), closed, false); err == nil {
		t.Fatal("a closed stream in the middle of a live session was hidden")
	}
	if err := finishSSHProxy(canceled, io.ErrUnexpectedEOF, true); err == nil {
		t.Fatal("another failure was hidden")
	}
}

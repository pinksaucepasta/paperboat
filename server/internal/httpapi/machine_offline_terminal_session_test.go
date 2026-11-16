package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

type fakeMachineVerifier struct {
	machine string
	err     error
	calls   int
	path    string
	body    string
}

func (f *fakeMachineVerifier) VerifyMachineRequest(_ context.Context, token string, proof []byte, method, path string, body []byte) (controlplane.MachineRequestClaims, error) {
	f.calls++
	f.path, f.body = path, string(body)
	if f.err != nil {
		return controlplane.MachineRequestClaims{}, f.err
	}
	if token != "machine-token" || len(proof) == 0 || method != http.MethodPost {
		return controlplane.MachineRequestClaims{}, controlplane.ErrHelperProof
	}
	return controlplane.MachineRequestClaims{MachineID: f.machine}, nil
}

type fakeAdopter struct {
	account, machine, session string
	err                       error
	calls                     int
}

func (f *fakeAdopter) AdoptOfflineTerminalSession(_ context.Context, account, machine, session string) (usermachines.TerminalSession, error) {
	f.calls++
	f.account, f.machine, f.session = account, machine, session
	return usermachines.TerminalSession{ID: session}, f.err
}

func offlineTerminalRequest(machineInPath, body string, signed bool) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/machines/"+machineInPath+"/offline-terminal-sessions", strings.NewReader(body))
	request.SetPathValue("id", machineInPath)
	if signed {
		request.Header.Set("Authorization", "Bearer machine-token")
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
	}
	return request
}

// The host's registration of an offline-created session: a signed request from the machine registers the session for
// the account it names; another machine's session path, an unsigned request, a refused identity, a bad body, an
// account without terminal access (not found) and a session the host does not confirm (not found) are all refused.
func TestMachineOfflineTerminalSessionRegistersOnlyTheMachinesOwnConfirmedSession(t *testing.T) {
	const good = `{"session_id":"terminal_offline_a","account_id":"acct_1"}`
	cases := []struct {
		name           string
		machinePath    string
		body           string
		signed         bool
		verifierErr    error
		adoptErr       error
		wantStatus     int
		wantAdoptCalls int
	}{
		{"signed request from the machine", "machine_1", good, true, nil, nil, 200, 1},
		{"another machine's path", "machine_2", good, true, nil, nil, 403, 0},
		{"no proof", "machine_1", good, false, nil, nil, 401, 0},
		{"identity refused", "machine_1", good, true, controlplane.ErrHelperProof, nil, 401, 0},
		{"identity check unavailable", "machine_1", good, true, errors.New("database down"), nil, 503, 0},
		{"missing account", "machine_1", `{"session_id":"terminal_offline_a"}`, true, nil, nil, 400, 0},
		{"unknown field", "machine_1", `{"session_id":"terminal_offline_a","account_id":"acct_1","x":1}`, true, nil, nil, 400, 0},
		{"account without terminal access", "machine_1", good, true, nil, usermachines.ErrNotFound, 404, 1},
		{"session the host does not confirm", "machine_1", good, true, nil, usermachines.ErrTerminalSessionNotFound, 404, 1},
		{"machine offline", "machine_1", good, true, nil, usermachines.ErrMachineOffline, 409, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verifier := &fakeMachineVerifier{machine: "machine_1", err: c.verifierErr}
			adopter := &fakeAdopter{err: c.adoptErr}
			recorder := httptest.NewRecorder()
			machineOfflineTerminalSession(verifier, adopter)(recorder, offlineTerminalRequest(c.machinePath, c.body, c.signed))
			if recorder.Code != c.wantStatus || adopter.calls != c.wantAdoptCalls {
				t.Fatalf("status %d (%s), adopt calls %d; want %d and %d", recorder.Code, recorder.Body.String(), adopter.calls, c.wantStatus, c.wantAdoptCalls)
			}
			if c.wantStatus == 200 && (adopter.account != "acct_1" || adopter.machine != "machine_1" || adopter.session != "terminal_offline_a" || verifier.body != c.body) {
				t.Fatalf("adopted account %q machine %q session %q after verifying body %q", adopter.account, adopter.machine, adopter.session, verifier.body)
			}
		})
	}
}

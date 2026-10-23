package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

const machineEventsIssuer = "https://api.example.test"

// verifyMachineEventsAnswer checks the answer header against the provider's published key and returns its claims.
func verifyMachineEventsAnswer(t *testing.T, provider *mint.Provider, header, body string) map[string]any {
	t.Helper()
	parts := strings.Split(header, ".")
	if len(parts) != 3 {
		t.Fatalf("answer header = %q", header)
	}
	var head struct {
		Type string `json:"typ"`
		KID  string `json:"kid"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(raw, &head); err != nil || head.Type != mint.MachineEventsType {
		t.Fatalf("answer header = %s", raw)
	}
	jwks, _ := provider.JWKSJSON()
	var trust struct {
		Keys []struct {
			KID string `json:"kid"`
			X   string `json:"x"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &trust); err != nil {
		t.Fatal(err)
	}
	for _, key := range trust.Keys {
		public, _ := base64.RawURLEncoding.DecodeString(key.X)
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if key.KID == head.KID && ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
			payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(body))
			if claims["request_sha256"] != hex.EncodeToString(sum[:]) {
				t.Fatalf("answer is bound to another request: %v", claims["request_sha256"])
			}
			return claims
		}
	}
	t.Fatal("answer signature does not verify against the published trust document")
	return nil
}

func testMachineEventSigning(t *testing.T) (MachineEventSigning, *mint.Provider) {
	t.Helper()
	provider, err := mint.NewEphemeral(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	signing, err := NewMachineEventSigning(provider, machineEventsIssuer)
	if err != nil {
		t.Fatal(err)
	}
	return signing, provider
}

type machineEventWaiterFake struct {
	machine string
	known   map[string]int64
	maxWait time.Duration
	changed map[string]int64
	calls   int
	gone    bool
}

func (f *machineEventWaiterFake) Wait(ctx context.Context, machineID string, known map[string]int64, maxWait time.Duration) (map[string]int64, error) {
	f.calls++
	f.machine, f.known, f.maxWait = machineID, known, maxWait
	if f.gone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.changed, nil
}

func TestMachineEventsWaitAuthenticatesAndAnswersChangesOrNothing(t *testing.T) {
	claims := controlplane.MachineRequestClaims{MachineID: "machine_01", UserID: "account_01", OperationID: "events_01", InstallationGeneration: 1}
	signing, provider := testMachineEventSigning(t)
	send := func(waiter *machineEventWaiterFake, verifier bootstrapVerifier, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		handler := machineEventsWait(waiter, verifier, signing)
		request := httptest.NewRequest(http.MethodPost, MachineEventsPath, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Paperboat-Machine-Identity", strings.Repeat("m", 48))
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
		request.Header.Set("Idempotency-Key", "events_01")
		if mutate != nil {
			mutate(request)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	body := `{"topics":{"previews":4,"machine_access":0}}`

	changed := &machineEventWaiterFake{changed: map[string]int64{"previews": 5}}
	if r := send(changed, bootstrapVerifier{claims: claims}, body, nil); r.Code != http.StatusOK || strings.TrimSpace(r.Body.String()) != `{"topics":{"previews":5}}` ||
		changed.machine != "machine_01" || changed.known["previews"] != 4 || changed.maxWait != machineevents.MaxWait {
		t.Fatalf("changed: status=%d body=%s waiter=%+v", r.Code, r.Body.String(), changed)
	} else if got := verifyMachineEventsAnswer(t, provider, r.Header().Get(MachineEventsAnswerHeader), body); got["kind"] != "changed" || got["sub"] != "machine_01" {
		t.Fatalf("changed answer claims = %v", got)
	}
	idle := &machineEventWaiterFake{changed: map[string]int64{}}
	if r := send(idle, bootstrapVerifier{claims: claims}, body, nil); r.Code != http.StatusNoContent || r.Body.Len() != 0 {
		t.Fatalf("no change: status=%d body=%q", r.Code, r.Body.String())
	} else if got := verifyMachineEventsAnswer(t, provider, r.Header().Get(MachineEventsAnswerHeader), body); got["kind"] != "unchanged" {
		t.Fatalf("unchanged answer must be signed too: %v", got)
	}
	// A host that hangs up before the answer is logged as 499, not as a silent 200.
	gone := &machineEventWaiterFake{gone: true}
	if r := send(gone, bootstrapVerifier{claims: claims}, body, func(r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		*r = *r.WithContext(ctx)
	}); r.Code != statusClientClosedRequest {
		t.Fatalf("client gone: status=%d", r.Code)
	}
	reject := func(name string, want int, verifier bootstrapVerifier, body string, mutate func(*http.Request)) {
		t.Helper()
		waiter := &machineEventWaiterFake{}
		if r := send(waiter, verifier, body, mutate); r.Code != want || waiter.calls != 0 {
			t.Errorf("%s: status=%d waited=%v, want %d and no wait (%s)", name, r.Code, waiter.calls != 0, want, r.Body.String())
		}
	}
	reject("wrong content type", http.StatusUnsupportedMediaType, bootstrapVerifier{claims: claims}, body, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	reject("no topics field", http.StatusBadRequest, bootstrapVerifier{claims: claims}, `{}`, nil)
	reject("unknown topic", http.StatusBadRequest, bootstrapVerifier{claims: claims}, `{"topics":{"billing":1}}`, nil)
	reject("negative generation", http.StatusBadRequest, bootstrapVerifier{claims: claims}, `{"topics":{"previews":-1}}`, nil)
	reject("unknown field", http.StatusBadRequest, bootstrapVerifier{claims: claims}, `{"topics":{},"x":1}`, nil)
	reject("no proof", http.StatusUnauthorized, bootstrapVerifier{claims: claims}, body, func(r *http.Request) { r.Header.Del("X-Paperboat-Machine-Proof") })
	other := claims
	other.OperationID = "another_operation"
	reject("proof for another operation", http.StatusForbidden, bootstrapVerifier{claims: other}, body, nil)
	other = claims
	other.MachineID = ""
	reject("no machine in the proof", http.StatusForbidden, bootstrapVerifier{claims: other}, body, nil)
}

type machineLookupFake struct{ owner, machine string }

func (f machineLookupFake) Get(_ context.Context, userID, machineID string) (usermachines.UserMachine, error) {
	if userID != f.owner || machineID != f.machine {
		return usermachines.UserMachine{}, usermachines.ErrNotFound
	}
	return usermachines.UserMachine{ID: machineID}, nil
}

func TestMachineEventsWaitForUserIsLimitedToTheUsersOwnMachine(t *testing.T) {
	signing, _ := testMachineEventSigning(t)
	send := func(user, machine string) (*httptest.ResponseRecorder, *machineEventWaiterFake) {
		waiter := &machineEventWaiterFake{changed: map[string]int64{"inventory": 3}}
		mux := http.NewServeMux()
		mux.Handle("POST /v1/machines/{machine_id}/events/wait", machineEventsWaitForUser(machineLookupFake{owner: "account_01", machine: "machine_01"}, waiter, signing))
		request := httptest.NewRequest(http.MethodPost, "/v1/machines/"+machine+"/events/wait", strings.NewReader(`{"topics":{"inventory":1}}`))
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: user}}))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response, waiter
	}
	if r, waiter := send("account_01", "machine_01"); r.Code != http.StatusOK || waiter.machine != "machine_01" || waiter.known["inventory"] != 1 {
		t.Fatalf("own machine: status=%d body=%s waiter=%+v", r.Code, r.Body.String(), waiter)
	}
	for name, args := range map[string][2]string{"another account": {"account_02", "machine_01"}, "another machine": {"account_01", "machine_02"}} {
		if r, waiter := send(args[0], args[1]); r.Code != http.StatusNotFound || waiter.calls != 0 {
			t.Errorf("%s: status=%d waited=%v, want 404 and no wait", name, r.Code, waiter.calls != 0)
		}
	}
	for path, want := range map[string]bool{MachineEventsPath: true, "/v1/machines/machine_01/events/wait": true, "/v1/machines/a/b/events/wait": false, "/v1/machines/machine_01": false} {
		if isMachineEventsWait(path) != want {
			t.Errorf("isMachineEventsWait(%q) = %v, want %v", path, !want, want)
		}
	}
}

// Released clients give up on a response after 30 s of waiting for headers. The server must answer a
// held wait before that, with room for the round trip, or no client ever sees its 204.
func TestMachineEventsAnswerBeforeReleasedClientsGiveUp(t *testing.T) {
	if machineevents.MaxWait+5*time.Second > machineevents.ClientHeaderTimeout {
		t.Fatalf("MaxWait %s leaves under 5 s before the clients' %s header timeout", machineevents.MaxWait, machineevents.ClientHeaderTimeout)
	}
}

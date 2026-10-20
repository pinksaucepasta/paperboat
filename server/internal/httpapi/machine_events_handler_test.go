package httpapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type machineEventWaiterFake struct {
	machine string
	known   map[string]int64
	maxWait time.Duration
	changed map[string]int64
	calls   int
}

func (f *machineEventWaiterFake) Wait(_ context.Context, machineID string, known map[string]int64, maxWait time.Duration) (map[string]int64, error) {
	f.calls++
	f.machine, f.known, f.maxWait = machineID, known, maxWait
	return f.changed, nil
}

func TestMachineEventsWaitAuthenticatesAndAnswersChangesOrNothing(t *testing.T) {
	claims := controlplane.MachineRequestClaims{MachineID: "machine_01", UserID: "account_01", OperationID: "events_01", InstallationGeneration: 1}
	send := func(waiter *machineEventWaiterFake, verifier bootstrapVerifier, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		handler := machineEventsWait(waiter, verifier)
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
		changed.machine != "machine_01" || changed.known["previews"] != 4 || changed.maxWait != 30*time.Second {
		t.Fatalf("changed: status=%d body=%s waiter=%+v", r.Code, r.Body.String(), changed)
	}
	idle := &machineEventWaiterFake{changed: map[string]int64{}}
	if r := send(idle, bootstrapVerifier{claims: claims}, body, nil); r.Code != http.StatusNoContent || r.Body.Len() != 0 {
		t.Fatalf("no change: status=%d body=%q", r.Code, r.Body.String())
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
	send := func(user, machine string) (*httptest.ResponseRecorder, *machineEventWaiterFake) {
		waiter := &machineEventWaiterFake{changed: map[string]int64{"inventory": 3}}
		mux := http.NewServeMux()
		mux.Handle("POST /v1/machines/{machine_id}/events/wait", machineEventsWaitForUser(machineLookupFake{owner: "account_01", machine: "machine_01"}, waiter))
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

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/machinelatency"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type fakeMachineLookup struct {
	machine usermachines.UserMachine
	err     error
	asked   []string
}

func (f *fakeMachineLookup) Get(_ context.Context, account, machine string) (usermachines.UserMachine, error) {
	f.asked = append(f.asked, account+"/"+machine)
	return f.machine, f.err
}

type fakeLatency struct {
	saved []machinelatency.RelaySample
	err   error
	items []machinelatency.Item
}

func (f *fakeLatency) SaveRelays(_ context.Context, _, _ string, samples []machinelatency.RelaySample) error {
	f.saved = samples
	return f.err
}

func (f *fakeLatency) List(context.Context, string) ([]machinelatency.Item, error) { return f.items, nil }

func latencyRequest(method, body string) *http.Request {
	request := httptest.NewRequest(method, "/v1/machines/m1/network-latency", strings.NewReader(body))
	request.SetPathValue("machine_id", "m1")
	return request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "account_1"}}))
}

func TestMachineLatencyReportRequiresAManageableMachineAndValidInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine usermachines.UserMachine
		lookup  error
		save    error
		body    string
		status  int
	}{
		{"accepted", usermachines.UserMachine{ID: "m1", CanManage: true}, nil, nil, `{"relays":[{"node_id":"node_a","rtt_ms":12}]}`, http.StatusNoContent},
		{"read-only viewer", usermachines.UserMachine{ID: "m1"}, nil, nil, `{"relays":[{"node_id":"node_a","rtt_ms":12}]}`, http.StatusForbidden},
		{"unknown machine", usermachines.UserMachine{}, usermachines.ErrNotFound, nil, `{"relays":[{"node_id":"node_a","rtt_ms":12}]}`, http.StatusNotFound},
		{"rejected sample", usermachines.UserMachine{ID: "m1", CanManage: true}, nil, machinelatency.ErrInvalid, `{"relays":[{"node_id":"node_a","rtt_ms":99999}]}`, http.StatusBadRequest},
		{"unknown field", usermachines.UserMachine{ID: "m1", CanManage: true}, nil, nil, `{"relays":[],"edges":[]}`, http.StatusBadRequest},
		{"not json", usermachines.UserMachine{ID: "m1", CanManage: true}, nil, nil, `nope`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			machineLatencyReport(&fakeMachineLookup{machine: tc.machine, err: tc.lookup}, &fakeLatency{err: tc.save})(recorder, latencyRequest(http.MethodPut, tc.body))
			if recorder.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", recorder.Code, recorder.Body.String(), tc.status)
			}
		})
	}
}

func TestMachineLatencyListChecksTheMachineBeforeListing(t *testing.T) {
	lookup := &fakeMachineLookup{machine: usermachines.UserMachine{ID: "m1"}}
	latency := &fakeLatency{items: []machinelatency.Item{{NodeID: "node_a", Role: "edge", Region: "india", RTTMillis: 21}}}
	recorder := httptest.NewRecorder()
	machineLatencyList(lookup, latency)(recorder, latencyRequest(http.MethodGet, ""))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"rtt_ms":21`) || len(lookup.asked) != 1 || lookup.asked[0] != "account_1/m1" {
		t.Fatalf("status=%d body=%s asked=%v", recorder.Code, recorder.Body.String(), lookup.asked)
	}
	recorder = httptest.NewRecorder()
	machineLatencyList(&fakeMachineLookup{err: usermachines.ErrNotFound}, latency)(recorder, latencyRequest(http.MethodGet, ""))
	if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), "node_a") {
		t.Fatalf("an invisible machine leaked latency: %d %s", recorder.Code, recorder.Body.String())
	}
}

package main

import (
	"context"
	"net/url"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

type fakeInspectorDaemon struct{ got localapi.InspectorRequest }

func (f *fakeInspectorDaemon) InspectorRequest(_ context.Context, request localapi.InspectorRequest) (localapi.InspectorResult, error) {
	f.got = request
	return localapi.InspectorResult{Status: 200, Payload: []byte(`{"ok":true}`)}, nil
}

func TestInspectorCallsReachTheMachineThroughTheDaemonOnly(t *testing.T) {
	daemon := &fakeInspectorDaemon{}
	client := &inspectorNativeClient{daemon: daemon, access: api.InspectorAccess{MachineID: "machine_1", ResourceKind: "tunnel", ResourceID: "tun_1", RouteID: "route_1"}}
	payload, status, err := client.do(context.Background(), "grant", "PUT", "/v1/inspector/policy", map[string]any{"enabled": true}, url.Values{"limit": {"5"}})
	if err != nil || status != 200 || string(payload) != `{"ok":true}` {
		t.Fatalf("payload=%q status=%d err=%v", payload, status, err)
	}
	if daemon.got.Access.MachineID != "machine_1" || daemon.got.Method != "PUT" || daemon.got.Query != "limit=5" || len(daemon.got.Body) == 0 || daemon.got.Grant != "grant" {
		t.Fatalf("daemon received %+v", daemon.got)
	}
}

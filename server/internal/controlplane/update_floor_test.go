package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/clientupdate"
)

type fixedFloor clientupdate.Policy

func (f fixedFloor) Get(context.Context) (clientupdate.Policy, error) {
	return clientupdate.Policy(f), nil
}

// A node reporting a release below its component's floor is told 426 update_required with how to update,
// before any state changes; a development build and a node at the floor are not gated (they reach the store,
// which this test leaves nil, so they panic out of the handler instead of answering 426).
func TestNodeBelowUpdateFloorIsToldToUpdate(t *testing.T) {
	service := NewEdgeService(nil, "edge-control-credential-01234567890123456789")
	service.SetUpdateFloors(map[string]NodeUpdateFloor{
		"edge":  fixedFloor{MinimumVersion: "2026.11.18.1", Reason: "carrier fix"},
		"relay": fixedFloor{MinimumVersion: "2026.11.18.1", Reason: "carrier fix"},
	})
	call := func(handler http.HandlerFunc, body string) (code int, envelope map[string]any) {
		defer func() {
			if recover() != nil {
				code = -1 // reached the (nil) store: not gated
			}
		}()
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler(response, request)
		_ = json.Unmarshal(response.Body.Bytes(), &envelope)
		return response.Code, envelope
	}
	edge := `{"edge_node_id":"node_a","process_epoch":"e","at":"2026-11-18T00:00:00Z","metrics":{"version":"%s"}}`
	relay := `{"node_id":"node_r","generation":1,"process_epoch":"e","metrics":{"version":"%s"}}`
	for name, handler := range map[string]struct {
		h    http.HandlerFunc
		body string
	}{"edge": {service.handleHeartbeat, edge}, "relay": {service.handleRelayObserve, relay}} {
		code, envelope := call(handler.h, strings.Replace(handler.body, "%s", "2026.11.16.2", 1))
		message, _ := envelope["message"].(string)
		if code != http.StatusUpgradeRequired || envelope["code"] != "update_required" || envelope["minimum_version"] != "2026.11.18.1" ||
			!strings.Contains(message, "Update required") || !strings.Contains(message, "pbh install") || !strings.Contains(message, "carrier fix") {
			t.Fatalf("%s below floor: %d %#v", name, code, envelope)
		}
		for _, version := range []string{"2026.11.18.1", "development"} {
			if code, _ := call(handler.h, strings.Replace(handler.body, "%s", version, 1)); code == http.StatusUpgradeRequired {
				t.Fatalf("%s %s gated", name, version)
			}
		}
	}
}

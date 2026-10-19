package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/tunnelv1"
)

type originHealthRecorderFake struct {
	calls  int
	tunnel string
	routes []tunnelv1.OriginRouteReport
}

func (f *originHealthRecorderFake) RecordOriginHealth(_ context.Context, _, tunnelID, _, _, _ string, routes []tunnelv1.OriginRouteReport, _ time.Time) error {
	f.calls++
	f.tunnel, f.routes = tunnelID, routes
	return nil
}

func TestConnectorOriginHealthRequiresTheExactMachineAndSession(t *testing.T) {
	now := time.Now().UTC()
	active := connectorprotocol.ActiveControlSession{
		AccountID: "account_01", TunnelID: "tunnel_01", ConnectorID: "connector_01", HostID: "host_01", SessionID: "session_01",
		ProcessGeneration: 2, ConfigGeneration: 4, ConfigContentHash: "sha256:" + strings.Repeat("a", 64),
	}
	claims := controlplane.MachineRequestClaims{MachineID: active.HostID, UserID: active.AccountID, OperationID: "origin_health_01", InstallationGeneration: 1}
	send := func(handler http.Handler, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tunnels/tunnel_01/connectors/connector_01/origin-health", strings.NewReader(body))
		request.SetPathValue("tunnel_id", active.TunnelID)
		request.SetPathValue("connector_id", active.ConnectorID)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Paperboat-Machine-Identity", strings.Repeat("m", 48))
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString([]byte("proof")))
		request.Header.Set("Idempotency-Key", "origin_health_01")
		if mutate != nil {
			mutate(request)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	report := connectorprotocol.OriginHealthRequest{
		Schema: connectorprotocol.OriginHealthSchema, SessionID: active.SessionID, ProcessGeneration: active.ProcessGeneration,
		ConfigGeneration: active.ConfigGeneration, ConfigContentHash: active.ConfigContentHash,
		Routes: []connectorprotocol.OriginHealthRoute{{RouteID: "route_01", State: "unreachable", Code: "refused", ObservedAt: now}},
	}
	body, _ := json.Marshal(report)
	recorder := &originHealthRecorderFake{}
	handler := connectorOriginHealth(bootstrapSessionReader{value: active, ok: true}, recorder, bootstrapVerifier{claims: claims}, func() time.Time { return now })

	if response := send(handler, string(body), nil); response.Code != http.StatusNoContent || recorder.calls != 1 || recorder.tunnel != "tunnel_01" || len(recorder.routes) != 1 || recorder.routes[0].State != "unreachable" || recorder.routes[0].Code != "refused" {
		t.Fatalf("accepted report: status=%d calls=%d routes=%+v", response.Code, recorder.calls, recorder.routes)
	}
	reject := func(name string, want int, h http.Handler, body string, mutate func(*http.Request)) {
		t.Helper()
		before := recorder.calls
		if response := send(h, body, mutate); response.Code != want || recorder.calls != before {
			t.Errorf("%s: status=%d recorded=%v, want %d and nothing recorded (%s)", name, response.Code, recorder.calls != before, want, response.Body.String())
		}
	}
	reject("wrong content type", http.StatusUnsupportedMediaType, handler, string(body), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	reject("unknown field", http.StatusBadRequest, handler, strings.Replace(string(body), `"schema"`, `"extra":1,"schema"`, 1), nil)
	reject("empty routes", http.StatusBadRequest, handler, strings.Replace(string(body), `"routes":[`, `"routes":[]`+`,"x":[`, 1), nil)
	reject("unknown session", http.StatusConflict, connectorOriginHealth(bootstrapSessionReader{value: active, ok: false}, recorder, bootstrapVerifier{claims: claims}, func() time.Time { return now }), string(body), nil)
	reject("no proof", http.StatusUnauthorized, handler, string(body), func(r *http.Request) { r.Header.Del("X-Paperboat-Machine-Proof") })
	other := claims
	other.MachineID = "another_machine"
	reject("another machine", http.StatusForbidden, connectorOriginHealth(bootstrapSessionReader{value: active, ok: true}, recorder, bootstrapVerifier{claims: other}, func() time.Time { return now }), string(body), nil)
	other = claims
	other.OperationID = "different_operation"
	reject("proof for another operation", http.StatusForbidden, connectorOriginHealth(bootstrapSessionReader{value: active, ok: true}, recorder, bootstrapVerifier{claims: other}, func() time.Time { return now }), string(body), nil)
}

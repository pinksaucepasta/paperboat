package control

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/usage"
)

const testControlCredential = "edge-control-test-012345678901234567890123456789"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func controlClient(t *testing.T, roundTrip roundTripFunc) *HTTPClient {
	t.Helper()
	client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second, Client: &http.Client{Transport: roundTrip}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func usageKey() usage.Key {
	return usage.Key{Node: "edge", Epoch: "epoch", Environment: "env", Route: "route", Revision: 1, Direction: "egress"}
}

func TestHTTPClientTypedOperationsAndAuthentication(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+testControlCredential {
			t.Errorf("unsafe auth request: %s %v", r.URL.String(), r.Header)
			return response(http.StatusUnauthorized, ""), nil
		}
		switch r.URL.Path {
		case "/v1/edge/assignments/current":
			data, _ := json.Marshal(map[string]any{"connector_generation": 3, "edge_pool": "default", "edge_node_id": "edge_1", "revoked": false})
			return response(http.StatusOK, string(data)), nil
		case "/v1/edge/usage-reports":
			var wire map[string]any
			if err := json.NewDecoder(r.Body).Decode(&wire); err != nil || wire["edge_node_id"] != "edge" || wire["route_id"] != "route" || wire["counter_epoch"] != "epoch" {
				t.Errorf("usage wire = %#v", wire)
			}
			data, _ := json.Marshal(UsageResult{Disposition: "accounted", Delta: 10})
			return response(http.StatusOK, string(data)), nil
		case "/v1/nodes/register":
			var registration NodeRegistration
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil || registration.CarrierEndpoint.Host != "edge.example.test" || registration.CarrierEndpoint.TCPPort != 27443 || registration.CarrierEndpoint.QUICPort != 27444 || !ValidCarrierServerSPKISHA256(registration.CarrierServerSPKISHA256) || registration.CarrierServerCertificateChainPEM == "" {
				t.Errorf("registration wire = %+v, %v", registration, err)
			}
			return response(http.StatusNoContent, ""), nil
		case "/v1/nodes/heartbeat":
			return response(http.StatusNoContent, ""), nil
		case "/v1/edge/routes/desired-state":
			return response(http.StatusOK, `{"routes":[{"route_id":"route","route_revision":2,"environment_id":"env","connector_generation":3,"edge_node_id":"edge","kind":"runtime_https_wss","public_host":"app.example.test","target":{"host":"127.0.0.1","port":8080}}]}`), nil
		case "/v1/edge/routes/observations":
			return response(http.StatusNoContent, ""), nil
		case "/v1/trust/revocations":
			if r.Method != http.MethodGet {
				t.Errorf("revocation method = %s", r.Method)
			}
			return response(http.StatusOK, `{"jtis":["jti_revoked"],"environments":[],"helper_generations":[],"key_ids":[]}`), nil
		default:
			return response(http.StatusNotFound, ""), nil
		}
	})
	current, err := client.Current(context.Background(), "env", "machine", "runtime")
	if err != nil || current.Generation != 3 || current.EdgeNode != "edge_1" {
		t.Fatalf("current = %+v, %v", current, err)
	}
	result, err := client.ReportUsage(context.Background(), UsageReport{OperationID: "op", Key: usageKey(), Interval: [2]time.Time{time.Unix(1, 0), time.Unix(2, 0)}})
	if err != nil || result.Delta != 10 {
		t.Fatalf("usage = %+v, %v", result, err)
	}
	trust, err := NewProcessCarrierServerTrust("edge", "epoch", "edge.example.test", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterNode(context.Background(), NodeRegistration{NodeID: "edge", ProcessEpoch: "epoch", Capacity: 1, CarrierEndpoint: ConnectorEndpoint{Host: "edge.example.test", TCPPort: 27443, QUICPort: 27444}, CarrierServerSPKISHA256: trust.SPKISHA256, CarrierServerCertificateChainPEM: trust.CertificateChainPEM}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil {
		t.Fatal(err)
	}
	routes, err := client.DesiredRoutes(context.Background(), "edge")
	if err != nil || len(routes) != 1 || routes[0].Revision != 2 {
		t.Fatalf("routes = %+v, %v", routes, err)
	}
	revocations, err := client.Revocations(context.Background())
	if err != nil || !strings.Contains(string(revocations), "jti_revoked") {
		t.Fatalf("revocations = %s, %v", revocations, err)
	}
	if err := client.ObserveRoutes(context.Background(), "edge", []RouteObservation{{RouteID: "route", RouteRevision: 2, EdgeNodeID: "edge", ConnectorGeneration: 3}}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientClassifiesStaleHeartbeat(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/nodes/heartbeat" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		return response(http.StatusConflict, `{"code":"node_observation_stale"}`), nil
	})
	if _, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); !errors.Is(err, ErrNodeObservationStale) {
		t.Fatalf("heartbeat error = %v", err)
	}
}

func TestHTTPClientAcceptsCanonicalRoutePublicTCPFields(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/edge/routes/desired-state" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		return response(http.StatusOK, `{"complete":true,"routes":[{"assignment_id":"assignment_1","route_id":"route_1","kind":"tunnel_http_wss","public_tcp_listener_id":null,"public_tcp_port":null},{"assignment_id":"assignment_2","route_id":"route_2","kind":"tunnel_tls","public_tcp_listener_id":"listener_tls_443","public_tcp_port":443}]}`), nil
	})
	snapshot, err := client.DesiredRouteSnapshot(context.Background(), "edge_1", "epoch_1")
	if err != nil || !snapshot.Canonical || !snapshot.Complete || len(snapshot.Routes) != 2 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	if snapshot.Routes[0].PublicTCPPort != 0 || snapshot.Routes[1].PublicTCPListenerID != "listener_tls_443" || snapshot.Routes[1].PublicTCPPort != 443 {
		t.Fatalf("public TCP projection = %+v", snapshot.Routes)
	}
}

func TestHTTPClientPropagatesOnlySentryTraceAndReportsRecovery(t *testing.T) {
	var calls int
	var outcomes []string
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second, ControlTrace: func(_ context.Context, operation string) (string, string, func(string, string)) {
		if operation != "dependency_health" {
			t.Fatalf("operation=%s", operation)
		}
		return "0123456789abcdef0123456789abcdef-0123456789abcdef-1", reference, func(outcome, code string) { outcomes = append(outcomes, outcome+":"+code) }
	}, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("sentry-trace") == "" || r.Header.Get("Support-Reference") != reference || r.Header.Get("baggage") != "" || r.Header.Get("traceparent") != "" {
			t.Fatalf("trace headers=%v", r.Header)
		}
		if calls == 1 {
			return response(http.StatusServiceUnavailable, ""), nil
		}
		return response(http.StatusNoContent, ""), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("failure=%v", err)
	}
	var failure *RequestFailure
	if !errors.As(err, &failure) || failure.SupportReference != reference || failure.Path != "/v1/nodes/heartbeat" || failure.Category != "http_status" {
		t.Fatalf("request failure lost safe correlation: %+v", failure)
	}
	if _, err = client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil {
		t.Fatalf("recovery=%v", err)
	}
	if got := strings.Join(outcomes, ","); got != "failed:unavailable,success:ok" {
		t.Fatalf("outcomes=%s", got)
	}
}

func TestHTTPClientPropagatesCorrelationWithoutTrace(t *testing.T) {
	const reference = "support_fedcba98-7654-4a32-8fed-cba987654321"
	client, err := NewHTTPClient(HTTPConfig{BaseURL: "https://control.test", Credential: testControlCredential, Timeout: time.Second, ControlTrace: func(context.Context, string) (string, string, func(string, string)) {
		return "", reference, func(string, string) {}
	}, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Support-Reference") != reference || r.Header.Get("sentry-trace") != "" {
			t.Fatalf("correlation headers=%v", r.Header)
		}
		return response(http.StatusNoContent, ""), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientNormalizesCanonicalManagedMatchType(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/edge/routes/desired-state" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		return response(http.StatusOK, `{"complete":true,"routes":[{"route_id":"rte_1","route_revision":1,"assignment_id":"asn_1","config_content_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","kind":"tunnel_http_wss","match_type":"managed","access_mode":"team","viewer_policy_generation":9}]}`), nil
	})
	snapshot, err := client.DesiredRouteSnapshot(context.Background(), "edge", "process_epoch")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Routes) != 1 || snapshot.Routes[0].MatchType != "managed_exact" || snapshot.Routes[0].AccessMode != "team" || snapshot.Routes[0].ViewerPolicyGeneration != 9 {
		t.Fatalf("routes = %+v", snapshot.Routes)
	}
}

func TestHTTPClientObservesViewerPolicyFieldsForReadyAndDetached(t *testing.T) {
	testCases := []struct {
		state string
	}{{state: "prepared"}, {state: "ready"}, {state: "detached"}}
	for _, testCase := range testCases {
		t.Run(testCase.state, func(t *testing.T) {
			client := controlClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/edge/routes/observations" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				var payload struct {
					Routes []RouteObservation `json:"routes"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Routes) != 1 || payload.Routes[0].ObservedState != testCase.state || payload.Routes[0].AccessMode != "private" || payload.Routes[0].ViewerPolicyGeneration != 12 {
					t.Fatalf("payload = %+v", payload)
				}
				return response(http.StatusNoContent, ""), nil
			})
			if err := client.ObserveRoutes(context.Background(), "edge_1", []RouteObservation{{RouteID: "route_1", RouteRevision: 3, AssignmentID: "assignment_1", AssignmentGeneration: 8, EdgeNodeID: "edge_1", EdgeProcessEpoch: "edge_epoch_1", ConnectorID: "connector_1", HostID: "host_1", ConnectorGeneration: 4, ConnectorSessionID: "session_1", ConnectorProcessGeneration: 4, ConfigGeneration: 5, ConfigContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AccessMode: "private", ViewerPolicyGeneration: 12, State: testCase.state, ObservedState: testCase.state}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHTTPClientRejectsPlaintextRedirectMalformedAndOversized(t *testing.T) {
	if _, err := NewHTTPClient(HTTPConfig{BaseURL: "http://control.test", Credential: testControlCredential, Timeout: time.Second}); !errors.Is(err, ErrControlInvalid) {
		t.Fatalf("plaintext = %v", err)
	}
	client := controlClient(t, func(*http.Request) (*http.Response, error) {
		result := response(http.StatusTemporaryRedirect, "")
		result.Header.Set("Location", "https://other.test/")
		return result, nil
	})
	if _, err := client.Current(context.Background(), "env", "machine", "runtime"); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("redirect = %v", err)
	}
	malformed := controlClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"connector_generation":3,"unknown":true}`), nil
	})
	if _, err := malformed.Current(context.Background(), "env", "machine", "runtime"); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("malformed = %v", err)
	}
	duplicate := controlClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"connector_generation":3,"connector_generation":4,"edge_pool":"default","edge_node_id":"edge"}`), nil
	})
	if _, err := duplicate.Current(context.Background(), "env", "machine", "runtime"); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("duplicate = %v", err)
	}
	oversized := controlClient(t, func(*http.Request) (*http.Response, error) {
		return response(http.StatusOK, strings.Repeat("x", maxControlDocument+1)), nil
	})
	if _, err := oversized.Current(context.Background(), "env", "machine", "runtime"); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("oversized = %v", err)
	}
	if _, err := oversized.Revocations(context.Background()); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("oversized revocations = %v", err)
	}
}

func TestHTTPClientPrivateAccessAdmissionsBindsProcessEpochHeader(t *testing.T) {
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != PrivateAccessCarrierAdmissionsPath || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testControlCredential || r.Header.Get("X-Paperboat-Edge-Node-ID") != "edge_1" || r.Header.Get("X-Paperboat-Edge-Process-Epoch") != "edge_epoch_1" {
			t.Fatalf("private admission headers = %s auth=%q node=%q epoch=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Edge-Node-ID"), r.Header.Get("X-Paperboat-Edge-Process-Epoch"))
		}
		return response(http.StatusUnauthorized, `{"code":"edge_identity_invalid"}`), nil
	})
	if _, err := client.PrivateAccessCarrierAdmissions(context.Background(), "edge_1", "edge_epoch_1"); !errors.Is(err, ErrControlUnavailable) {
		t.Fatalf("private admissions error = %v", err)
	}
}

func TestExpiredUsageAcknowledgementDrainsQueueAndResumes(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	q, _ := usage.NewQueue(1, 1<<20)
	old := now.Add(-usage.DeliveryWindow - time.Hour)
	report, err := usage.NewSignedReport("key", private, usage.SignedDocument{OperationID: "expired_op", Key: usage.Key{Authority: "grant:a", Node: "edge", Epoch: "old", Environment: "env", Route: "route", Revision: 1, Direction: "ingress"}, Bytes: 100, Start: old, End: old})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Enqueue(report); err != nil {
		t.Fatal(err)
	}
	disposition := "expired_not_billed"
	client := controlClient(t, func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{"delta":100,"disposition":"`+disposition+`"}`), nil
	})
	result, delivered, err := DeliverNext(context.Background(), q, client)
	if err != nil || !delivered || result.Disposition != disposition || q.Len() != 0 {
		t.Fatalf("expiry acknowledgement: %+v %v", result, err)
	}
	m := &usage.Meter{Node: "edge", Epoch: "new", Counters: usage.NewCounters(), Queue: q, KeyID: "key", PrivateKey: private, Persist: func() error { return nil }, Now: func() time.Time { return now }}
	if err = m.Record("env", "route", 1, 1, 0, "grant:b"); err != nil {
		t.Fatal(err)
	}
	if err = m.Flush(); err != nil {
		t.Fatal(err)
	}
	disposition = "unknown"
	if _, _, err = DeliverNext(context.Background(), q, client); err == nil || q.Len() != 1 {
		t.Fatal("unknown receipt discarded pending usage")
	}
	disposition = "accounted"
	result, delivered, err = DeliverNext(context.Background(), q, client)
	if err != nil || !delivered || result.Disposition != "accounted" || q.Len() != 0 {
		t.Fatalf("fresh delivery: %+v %v", result, err)
	}
}

func TestControlAttemptPreservesTypedCauseStatusAndRecovery(t *testing.T) {
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Support-Reference") != reference {
			t.Error("request reference missing")
		}
		calls++
		switch calls {
		case 1:
			w.WriteHeader(503)
			io.WriteString(w, "PRIVATE_RESPONSE_BODY")
		case 2:
			w.WriteHeader(409)
			io.WriteString(w, "PRIVATE_RESPONSE_BODY")
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	var failures []error
	var outcomes []string
	client, err := NewHTTPClient(HTTPConfig{BaseURL: server.URL, Credential: testControlCredential, Timeout: time.Second, Client: server.Client(), ControlTrace: func(context.Context, string) (string, string, func(string, string)) {
		return "", reference, func(outcome, code string) { outcomes = append(outcomes, outcome+":"+code) }
	}, ControlFailure: func(_ context.Context, ref string, err error) {
		if ref != reference {
			t.Error("attempt reference mismatch")
		}
		failures = append(failures, err)
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{503, 409} {
		_, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"})
		var failure *RequestFailure
		if !errors.As(err, &failure) || failure.DiagnosticStatus() != status || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatalf("unsafe or missing status: %v", err)
		}
		if status == 409 && !errors.Is(err, ErrNodeObservationStale) {
			t.Fatal("stale fencing was lost")
		}
	}
	if _, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil {
		t.Fatal("control did not recover", err)
	}
	if len(failures) != 2 || strings.Join(outcomes, ",") != "failed:control_request_failed,rejected:control_request_failed,success:ok" {
		t.Fatalf("attempt ownership: %v %d", outcomes, len(failures))
	}
	original := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	offline := controlClient(t, func(*http.Request) (*http.Response, error) { return nil, original })
	for _, revocations := range []bool{false, true} {
		var err error
		if revocations {
			_, err = offline.Revocations(context.Background())
		} else {
			_, err = offline.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"})
		}
		var cause *net.OpError
		if !errors.Is(err, ErrControlUnavailable) || !errors.Is(err, syscall.ECONNREFUSED) || !errors.As(err, &cause) || cause != original {
			t.Fatal("transport cause lost")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Heartbeat(canceled, NodeObservation{NodeID: "edge"}); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrControlUnavailable) {
		t.Fatal("cancellation cause lost")
	}
}

// Audit T11: the heartbeat answer carries the server's edge ingress limits; a server without
// them (204) leaves the answer empty, and an invalid set is refused, never applied.
func TestHeartbeatAnswerCarriesIngressLimits(t *testing.T) {
	answer := ""
	client := controlClient(t, func(*http.Request) (*http.Response, error) {
		if answer == "" {
			return response(http.StatusNoContent, ""), nil
		}
		return response(http.StatusOK, answer), nil
	})
	if got, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil || got.IngressLimits != nil {
		t.Fatalf("204 heartbeat = %+v, %v", got, err)
	}
	set := edgelimits.Default()
	set.Account.Connections = 500
	raw, err := json.Marshal(map[string]any{"ingress_limits": set})
	if err != nil {
		t.Fatal(err)
	}
	answer = string(raw)
	if got, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); err != nil || got.IngressLimits == nil || *got.IngressLimits != set {
		t.Fatalf("heartbeat answer = %+v, %v", got, err)
	}
	set.Account.Connections = 0
	raw, _ = json.Marshal(map[string]any{"ingress_limits": set})
	answer = string(raw)
	if got, err := client.Heartbeat(context.Background(), NodeObservation{NodeID: "edge"}); !errors.Is(err, ErrControlUnavailable) || got.IngressLimits != nil {
		t.Fatalf("invalid limits = %+v, %v", got, err)
	}
}

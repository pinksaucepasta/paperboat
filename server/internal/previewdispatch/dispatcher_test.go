package previewdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewv1"
)

type testRouteResolver struct {
	route MachineRoute
	err   error
	calls int
}

type dispatchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dispatchRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingDispatchBody struct{ err error }

func (r failingDispatchBody) Read([]byte) (int, error) { return 0, r.err }

type dispatchResponseBody struct {
	reader   io.Reader
	closeErr error
}

func (r dispatchResponseBody) Read(raw []byte) (int, error) { return r.reader.Read(raw) }
func (r dispatchResponseBody) Close() error                 { return r.closeErr }

func (r *testRouteResolver) ResolvePreviewDispatchRoute(context.Context, string, string) (MachineRoute, error) {
	r.calls++
	return r.route, r.err
}

func testDispatchRequest() previewv1.DispatchRequest {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	request := previewv1.DispatchRequest{
		Schema: previewv1.Schema, Kind: previewv1.PreviewDispatchKind,
		PreviewID: "prv_1", OperationID: "operation_1", AccountID: "acct_1", ActorID: "user_1",
		OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground",
		Target: previewv1.Target{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public",
		Endpoint: "https://abc.preview.example.test", LeaseDeadline: now.Add(time.Hour),
		LeaseETag: previewtunnelapi.ETag(previewv1.Kind, "prv_1", 1), State: "allocating",
		AllocationState: "pending", EdgeState: "pending", OriginState: "unknown",
		CreatedAt: now, LastRenewedAt: now, ExpectedGeneration: 1,
		IdempotencyKey: "create_1", RequestID: "req_1", CorrelationID: "cor_1",
	}
	hash, err := request.ComputeRequestHash()
	if err != nil {
		panic(err)
	}
	request.RequestHash = hash
	return request
}

func TestDecodeOutcomeRejectsOversizedWhitespace(t *testing.T) {
	request := testDispatchRequest()
	body, err := json.Marshal(previewv1.DispatchOutcome{
		Schema: previewv1.Schema, Kind: previewv1.PreviewDispatchKind,
		PreviewID: request.PreviewID, OperationID: request.OperationID,
		State: "accepted", Generation: request.ExpectedGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(strings.Repeat(" ", maxResponse))...)
	_, err = decodeOutcome(strings.NewReader(string(body)), request, 200)
	var dispatchErr *DispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "remote_invalid_response" || !dispatchErr.Uncertain || !dispatchErr.Retryable {
		t.Fatalf("error = %v, want bounded invalid response", err)
	}
}

func TestDecodeOutcomeRejectsDuplicateOperationID(t *testing.T) {
	request := testDispatchRequest()
	body := `{"schema":"paperboat.preview-tunnel/v1","kind":"preview_dispatch","preview_id":"prv_1","operation_id":"operation_1","operation_id":"operation_other","state":"accepted","generation":1}`
	_, err := decodeOutcome(strings.NewReader(body), request, 200)
	var dispatchErr *DispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "remote_invalid_response" || !dispatchErr.Uncertain || !dispatchErr.Retryable {
		t.Fatalf("error = %v, want duplicate-field rejection", err)
	}
}

func TestDispatchMintsExactSingleOperationCredentialAndPostsCanonicalRequest(t *testing.T) {
	for _, prefix := range []string{"", "/v1/runtime/machine.customer.test"} {
		t.Run(prefix, func(t *testing.T) {
			now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
			request := testDispatchRequest()
			provider, err := mint.NewEphemeral(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			var gotBody []byte
			var gotToken string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
				if incoming.Method != http.MethodPost || incoming.URL.Path != prefix+"/v1/preview-launches" {
					t.Errorf("request = %s %s", incoming.Method, incoming.URL.Path)
				}
				gotToken = strings.TrimPrefix(incoming.Header.Get("Authorization"), "Bearer ")
				gotBody, _ = io.ReadAll(incoming.Body)
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(previewv1.DispatchOutcome{Schema: previewv1.Schema, Kind: previewv1.PreviewDispatchKind, PreviewID: request.PreviewID, OperationID: request.OperationID, State: "accepted", Generation: request.ExpectedGeneration})
			}))
			defer server.Close()
			resolver := &testRouteResolver{route: MachineRoute{EnvironmentID: "env_1", BaseURL: server.URL + prefix}}
			dispatcher, err := New(Config{Resolver: resolver, Signer: provider, Issuer: "issuer", Client: server.Client(), Timeout: time.Second, Now: func() time.Time { return now }, NewJTI: func() (string, error) { return "jti_preview_1", nil }})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := dispatcher.Dispatch(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.State != "accepted" || resolver.calls != 1 || len(gotBody) == 0 || gotToken == "" {
				t.Fatalf("outcome=%#v resolver_calls=%d body=%d token=%t", outcome, resolver.calls, len(gotBody), gotToken != "")
			}
			var posted previewv1.DispatchRequest
			if err := json.Unmarshal(gotBody, &posted); err != nil {
				t.Fatal(err)
			}
			if posted != request {
				t.Fatalf("posted request changed canonical projection: got=%#v want=%#v", posted, request)
			}
			claims, err := provider.VerifyCredential(gotToken, "issuer", "preview_launch", now)
			if err != nil {
				t.Fatal(err)
			}
			if claims.AccountID != request.AccountID || claims.ActorID != request.ActorID || claims.MachineID != request.OwnerMachineID || claims.PreviewID != request.PreviewID || claims.OperationID != request.OperationID || claims.OwnerSessionID != request.OwnerSessionID || claims.OwnerSessionKind != request.OwnerSessionKind || claims.ExpectedGeneration != request.ExpectedGeneration || claims.IdempotencyKey != request.IdempotencyKey || claims.RequestID != request.RequestID || claims.CorrelationID != request.CorrelationID || claims.RequestHash != request.RequestHash {
				t.Fatalf("credential claims do not bind request: %#v", claims)
			}
			if claims.Endpoint != request.Endpoint || claims.TargetAddress != request.Target.Address {
				t.Fatalf("credential did not carry the exact safe bindings: endpoint=%q target=%q", claims.Endpoint, claims.TargetAddress)
			}

		})
	}
}

func TestDispatchClassifiesOfflineAndTimeoutAsTypedOutcomes(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	request := testDispatchRequest()
	provider, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	offlineResolver := &testRouteResolver{err: fmt.Errorf("route state: %w", ErrMachineOffline)}
	dispatcher, err := New(Config{Resolver: offlineResolver, Signer: provider, Issuer: "issuer", Timeout: time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	var offline *DispatchError
	if !errors.As(err, &offline) || offline.Code != "machine_unavailable" || offline.Uncertain {
		t.Fatalf("offline error = %#v", err)
	}
	secret := "private-route-detail"
	operationalResolver := &testRouteResolver{err: errors.Join(ErrMachineOffline, fmt.Errorf("query %s: %w", secret, syscall.ECONNREFUSED))}
	operationalDispatcher, err := New(Config{Resolver: operationalResolver, Signer: provider, Issuer: "issuer", Timeout: time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operationalDispatcher.Dispatch(context.Background(), request)
	var operational *DispatchError
	if !errors.As(err, &operational) || operational.Code != "route_resolution_failed" || !operational.Uncertain || !errors.Is(err, syscall.ECONNREFUSED) || !errors.Is(err, previewv1.ErrDispatchUncertain) {
		t.Fatalf("mixed offline and operational route error = %#v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("safe dispatch error exposed route detail: %v", err)
	}
	pureCancel := classifyTransportError(context.Background(), fmt.Errorf("request stopped: %w", context.Canceled))
	var pureCancelDispatch *DispatchError
	if !errors.As(pureCancel, &pureCancelDispatch) || pureCancelDispatch.Code != "canceled" {
		t.Fatalf("pure cancellation = %#v", pureCancel)
	}
	mixedCancel := classifyTransportError(context.Background(), errors.Join(context.Canceled, syscall.ECONNREFUSED))
	var mixedCancelDispatch *DispatchError
	if !errors.As(mixedCancel, &mixedCancelDispatch) || mixedCancelDispatch.Code != "transport_failed" || !errors.Is(mixedCancel, syscall.ECONNREFUSED) {
		t.Fatalf("mixed cancellation masked transport cause: %#v", mixedCancel)
	}
	deadlineCancel := classifyTransportError(context.Background(), errors.Join(context.Canceled, context.DeadlineExceeded))
	var deadlineDispatch *DispatchError
	if !errors.As(deadlineCancel, &deadlineDispatch) || deadlineDispatch.Code != "timeout" {
		t.Fatalf("deadline did not outrank cancellation: %#v", deadlineCancel)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		startOnce.Do(func() { close(started) })
		<-release
	}))
	defer server.Close()
	timeoutDispatcher, err := New(Config{Resolver: &testRouteResolver{route: MachineRoute{EnvironmentID: "env_1", BaseURL: server.URL}}, Signer: provider, Issuer: "issuer", Client: server.Client(), Timeout: 20 * time.Millisecond, Now: func() time.Time { return now }, NewJTI: func() (string, error) { return "jti_timeout", nil }})
	if err != nil {
		t.Fatal(err)
	}
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := timeoutDispatcher.Dispatch(context.Background(), request)
		dispatchDone <- dispatchErr
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timeout request did not reach the test server")
	}
	err = <-dispatchDone
	close(release)
	var timeoutErr *DispatchError
	if !errors.As(err, &timeoutErr) || timeoutErr.Code != "timeout" || !timeoutErr.Uncertain || !errors.Is(err, previewv1.ErrDispatchUncertain) {
		t.Fatalf("timeout error = %#v", err)
	}
	if strings.Contains(err.Error(), "jti_timeout") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("timeout error leaked sensitive transport material: %v", err)
	}
}

func TestDispatchResponseReadFailureRetainsCauseAndRetriesSameOperation(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	request := testDispatchRequest()
	provider, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	operations := make([]string, 0, 3)
	secret := "private-response-detail"
	closeSecret := "private-response-close-detail"
	client := &http.Client{Transport: dispatchRoundTripFunc(func(incoming *http.Request) (*http.Response, error) {
		attempts++
		body, readErr := io.ReadAll(incoming.Body)
		if readErr != nil {
			return nil, readErr
		}
		var posted previewv1.DispatchRequest
		if err := json.Unmarshal(body, &posted); err != nil {
			return nil, err
		}
		operations = append(operations, posted.OperationID)
		response := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header), Request: incoming}
		if attempts == 1 {
			readFailure := fmt.Errorf("response detail %s: %w", secret, syscall.ECONNRESET)
			response.Body = io.NopCloser(io.MultiReader(strings.NewReader("{"), failingDispatchBody{err: readFailure}))
			return response, nil
		}
		payload, err := json.Marshal(previewv1.DispatchOutcome{Schema: previewv1.Schema, Kind: previewv1.PreviewDispatchKind, PreviewID: request.PreviewID, OperationID: request.OperationID, State: "accepted", Generation: request.ExpectedGeneration})
		if err != nil {
			return nil, err
		}
		if attempts == 2 {
			response.Body = dispatchResponseBody{reader: strings.NewReader(string(payload)), closeErr: fmt.Errorf("response close detail %s: %w", closeSecret, syscall.EIO)}
		} else if attempts == 3 {
			response.Body = io.NopCloser(strings.NewReader(`{"schema":"` + previewv1.Schema + `","detail":"` + secret + `","state":]`))
		} else {
			response.Body = io.NopCloser(strings.NewReader(string(payload)))
		}
		return response, nil
	})}
	dispatcher, err := New(Config{Resolver: &testRouteResolver{route: MachineRoute{EnvironmentID: "env_1", BaseURL: "https://machine.example.test"}}, Signer: provider, Issuer: "issuer", Client: client, Timeout: time.Second, Now: func() time.Time { return now }, NewJTI: func() (string, error) { return "jti_response_retry", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	var dispatchErr *DispatchError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "remote_response_read_failed" || !dispatchErr.Uncertain || !dispatchErr.Retryable || !errors.Is(err, syscall.ECONNRESET) || !errors.Is(err, previewv1.ErrDispatchUncertain) {
		t.Fatalf("response read failure lost its cause or uncertainty: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("safe dispatch error exposed private response detail: %v", err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "remote_response_close_failed" || !dispatchErr.Uncertain || !dispatchErr.Retryable || !errors.Is(err, syscall.EIO) || !errors.Is(err, previewv1.ErrDispatchUncertain) {
		t.Fatalf("response close failure lost its cause or uncertainty: %v", err)
	}
	if strings.Contains(err.Error(), closeSecret) {
		t.Fatalf("safe dispatch error exposed private close detail: %v", err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "remote_invalid_response" || !dispatchErr.Uncertain || !dispatchErr.Retryable || !errors.As(err, &syntaxErr) || !errors.Is(err, previewv1.ErrDispatchUncertain) {
		t.Fatalf("invalid response lost its decoder cause or uncertainty: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("safe dispatch error exposed private response detail: %v", err)
	}
	outcome, err := dispatcher.Dispatch(context.Background(), request)
	if err != nil || outcome.State != "accepted" {
		t.Fatalf("same-operation retry outcome=%+v err=%v", outcome, err)
	}
	if attempts != 4 || len(operations) != 4 || operations[0] != request.OperationID || operations[1] != request.OperationID || operations[2] != request.OperationID || operations[3] != request.OperationID {
		t.Fatalf("retry changed operation identity: attempts=%d operations=%v", attempts, operations)
	}
}

func TestDispatchMalformedRouteRetainsParseCauseWithoutLeakingRoute(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	request := testDispatchRequest()
	provider, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	secret := "private-route-secret"
	dispatcher, err := New(Config{Resolver: &testRouteResolver{route: MachineRoute{EnvironmentID: "env_1", BaseURL: "https://user:" + secret + "@machine.example.test/%zz"}}, Signer: provider, Issuer: "issuer", Timeout: time.Second, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	var dispatchErr *DispatchError
	var parseErr *url.Error
	if !errors.As(err, &dispatchErr) || dispatchErr.Code != "route_unavailable" || !errors.Is(err, ErrRouteUnavailable) || !errors.As(err, &parseErr) {
		t.Fatalf("malformed route lost its typed parse cause: %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("safe dispatch error exposed route credentials: %v", err)
	}
}

func TestDispatchRejectsRedirectWithoutForwardingBearer(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	request := testDispatchRequest()
	provider, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		forwarded <- incoming.Header.Get("Authorization")
		writer.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		http.Redirect(writer, incoming, target.URL+"/v1/preview-launches", http.StatusFound)
	}))
	defer redirect.Close()
	dispatcher, err := New(Config{Resolver: &testRouteResolver{route: MachineRoute{EnvironmentID: "env_1", BaseURL: redirect.URL}}, Signer: provider, Issuer: "issuer", Client: redirect.Client(), Timeout: time.Second, Now: func() time.Time { return now }, NewJTI: func() (string, error) { return "jti_redirect", nil }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.Dispatch(context.Background(), request)
	var dispatchErr *DispatchError
	if !errors.As(err, &dispatchErr) || !dispatchErr.Uncertain {
		t.Fatalf("redirect error = %#v", err)
	}
	select {
	case bearer := <-forwarded:
		t.Fatalf("redirect forwarded bearer %q", bearer)
	default:
	}
}

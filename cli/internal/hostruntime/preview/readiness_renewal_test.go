package preview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type publicationLeaseAuthority struct {
	*sessionLeaseClient
	mu       sync.Mutex
	lease    Lease
	renewals int
}

func (a *publicationLeaseAuthority) Renew(_ context.Context, lease Lease, _ string) (Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if lease.ETag != a.lease.ETag {
		return Lease{}, &api.APIError{Status: http.StatusPreconditionFailed, Code: "precondition_failed"}
	}
	a.lease.Generation++
	a.lease.ETag = formatLeaseETag(a.lease.ID, a.lease.Generation)
	a.renewals++
	return a.lease, nil
}

func TestPendingPublicationDoesNotLoseLeaseDuringRenewal(t *testing.T) {
	admitted := testDispatchRequest(t, time.Now().UTC()).Lease()
	admitted.Endpoint, admitted.EndpointState = "", "custom_domain_not_ready"
	authority := &publicationLeaseAuthority{sessionLeaseClient: &sessionLeaseClient{}, lease: admitted}
	observer, err := NewHTTPDispatchReadinessObserver(HTTPDispatchReadinessObserverConfig{
		ControlURL: "https://api.example.test", AllowedHosts: []string{"api.example.test"}, Identities: readinessTokenSource("machine-identity"),
		Proofs: &readinessProofSource{t: t, operation: "op_pending", path: "/v1/previews/prv_dispatch_1/readiness"},
		Transport: readinessRoundTripper(func(request *http.Request) (*http.Response, error) {
			authority.mu.Lock()
			defer authority.mu.Unlock()
			if request.Method == http.MethodPost {
				if request.Header.Get("If-Match") != authority.lease.ETag {
					return &http.Response{StatusCode: http.StatusPreconditionFailed, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				authority.lease.Generation++
				authority.lease.ETag = formatLeaseETag(admitted.ID, authority.lease.Generation)
			} else if authority.renewals > 0 {
				authority.lease = sessionReadyLease(authority.lease)
				authority.lease.Endpoint, authority.lease.EndpointState = "https://preview.example.test", "ready"
			}
			body, _ := json.Marshal(map[string]any{"data": authority.lease})
			header := make(http.Header)
			header.Set("ETag", authority.lease.ETag)
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	inner := &sessionCarrier{run: func(ctx context.Context, lease Lease, ready func(Lease) error) error {
		if err := ready(sessionReadyLease(lease)); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}}
	carrier := &readinessCarrier{inner: inner, observer: observer, admitted: admitted, readiness: DispatchReadiness{OperationID: "op_pending", IdempotencyKey: "pending-key", RequestID: "req_pending", CorrelationID: "cor_pending"}}
	session, err := StartExisting(t.Context(), SessionConfig{LeaseClient: authority, Carrier: carrier, OwnerMachineID: admitted.OwnerMachineID, OwnerSessionID: admitted.OwnerSessionID, OwnerSessionKind: admitted.OwnerSessionKind, Target: admitted.Target, AccessMode: admitted.AccessMode, DisableParentWatch: true, RenewInterval: 10 * time.Millisecond}, admitted)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop(context.Background())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ready, err := session.WaitReady(ctx)
	if err != nil || !isReadyLease(ready) {
		t.Fatalf("pending publication lost the lease instead of renewing its canonical ETag: %v", err)
	}
	authority.mu.Lock()
	renewals := authority.renewals
	authority.mu.Unlock()
	if renewals == 0 {
		t.Fatal("publication became ready without exercising renewal")
	}
}

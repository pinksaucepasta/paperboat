package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

type renewableCredentialSource func() (config.Credential, error)

func (f renewableCredentialSource) Credential() (config.Credential, error) { return f() }

func TestPreviewPollUsesCurrentAccountCredential(t *testing.T) {
	now := time.Now().UTC()
	lease := PreviewLease{Schema: PreviewTunnelSchemaV1, Kind: "preview_lease", ID: "prv_1", AccountID: "acct_1", ActorID: "actor_1", OwnerMachineID: "machine_1", OwnerSessionID: "session_1", OwnerSessionKind: "foreground", Target: PreviewLeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "public", EndpointState: "custom_domain_not_ready", LeaseDeadline: now.Add(time.Hour), State: "connecting", AllocationState: "pending", EdgeState: "pending", OriginState: "unknown", CreatedAt: now, LastRenewedAt: now}
	current := "old-access"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer "+current {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("ETag", `"ptv1:preview_lease:cHJ2XzE:1"`)
		writePreviewLeaseEnvelope(t, w, lease)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{AccessToken: current}, nil)
	sourceErr := error(nil)
	client.SetCredentialSource(renewableCredentialSource(func() (config.Credential, error) { return config.Credential{AccessToken: current}, sourceErr }))
	if _, err := client.GetPreviewLease(context.Background(), lease.ID); err != nil {
		t.Fatal(err)
	}
	// The old access token expires while the same preview command waits for DNS.
	current = "renewed-access"
	if _, err := client.GetPreviewLease(context.Background(), lease.ID); err != nil {
		t.Fatal("ongoing preview retained expired bearer", err)
	}
	sourceErr = errors.New("credential custody unavailable")
	before := requests
	if _, err := client.GetPreviewLease(context.Background(), lease.ID); !errors.Is(err, sourceErr) {
		t.Fatal("credential failure lost", err)
	}
	if requests != before {
		t.Fatal("credential failure sent stale bearer request")
	}
}

func TestMachineProofRequestDoesNotDependOnAccountRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer machine-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			t.Error("machine proof was replaced by account authentication")
		}
		writeData(w, http.StatusOK, map[string]any{})
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, nil)
	client.SetMachineAuth(machineAuthTestSource{})
	client.SetCredentialSource(renewableCredentialSource(func() (config.Credential, error) {
		t.Error("machine route requested account credentials")
		return config.Credential{}, errors.New("unavailable")
	}))
	if err := client.doWithHeaders(context.Background(), http.MethodPost, "/v1/previews", map[string]string{}, nil, http.Header{"Idempotency-Key": {"create-key"}}); err != nil {
		t.Fatal(err)
	}
}

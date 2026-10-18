package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/spf13/cobra"
)

// The authority delays DNS publication, while a real daemon Session renews its
// lease and the production CLI carrier observes the canonical resource.
type delayedPublicationAuthority struct {
	mu           sync.Mutex
	lease        preview.Lease
	ownerDone    <-chan struct{}
	host         *preview.Session
	renewals     int
	stops        int
	publishAfter time.Duration
}

func (a *delayedPublicationAuthority) Create(context.Context, preview.LeaseRequest) (preview.Lease, error) {
	panic("host must attach to the existing resource")
}
func (a *delayedPublicationAuthority) Renew(_ context.Context, _ preview.Lease, _ string) (preview.Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.renewals++
	a.lease.Generation++
	a.lease.ETag = fmt.Sprintf(`"ptv1:preview_lease:cHJ2X3B1Ymxpc2g:%d"`, a.lease.Generation)
	a.lease.LastRenewedAt = time.Now().UTC()
	a.lease.LeaseDeadline = a.lease.LastRenewedAt.Add(30 * time.Second)
	return a.lease, nil
}
func (a *delayedPublicationAuthority) Stop(context.Context, preview.Lease, string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops++
	return nil
}
func (a *delayedPublicationAuthority) Close(context.Context) error { return nil }
func (a *delayedPublicationAuthority) Run(ctx context.Context, _ preview.Lease, ready func(preview.Lease) error) error {
	timer := time.NewTimer(a.publishAfter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	a.mu.Lock()
	a.lease.State, a.lease.EdgeState = "ready", "ready"
	a.lease.Generation++
	a.lease.ETag = fmt.Sprintf(`"ptv1:preview_lease:cHJ2X3B1Ymxpc2g:%d"`, a.lease.Generation)
	lease := a.lease
	a.mu.Unlock()
	if err := ready(lease); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestPreviewManagedPublicationRetainsOwnerBeyondThirtySeconds(t *testing.T) {
	registry, err := preview.NewRuntimeOwnerSessionRegistry(preview.RuntimeOwnerSessionRegistryConfig{MachineID: "machine_cli", RuntimeDone: t.Context().Done()})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	owners, err := preview.NewOwnerSessionLeaseManager(preview.OwnerSessionLeaseManagerConfig{MachineID: "machine_cli", ControlToken: "local_control", Registry: registry, RunContext: t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	defer owners.Close()
	ownerHandler := &recordingOwnerLeaseHandler{next: owners}
	ownerServer := httptest.NewServer(ownerHandler)
	defer ownerServer.Close()
	ownerClient, err := preview.NewLocalOwnerSessionClient(ownerServer.URL, "local_control", ownerServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer origin.Close()
	authority := &delayedPublicationAuthority{publishAfter: 35 * time.Second}
	apiServer := newPreviewCommandServer(t, func(r *http.Request, body map[string]any) (any, string, int) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/previews" {
			owner := body["owner_session_id"].(string)
			target := body["target"].(map[string]any)
			deadline, err := time.Parse(time.RFC3339Nano, body["expires_at"].(string))
			if err != nil {
				t.Error(err)
			}
			now := time.Now().UTC()
			authority.lease = preview.Lease{Schema: preview.PreviewTunnelSchemaV1, Kind: preview.PreviewLeaseKind, ID: "prv_publish", AccountID: "acct_cli", ActorID: "actor_cli", OwnerMachineID: "machine_cli", OwnerSessionID: owner, OwnerSessionKind: preview.OwnerSessionLocalLease, Target: preview.LeaseTarget{Scheme: target["scheme"].(string), Address: target["address"].(string)}, AccessMode: "public", Endpoint: "https://preview.example.test", EndpointState: "ready", State: "connecting", AllocationState: "ready", EdgeState: "pending", OriginState: "ready", CreatedAt: now, LastRenewedAt: now, LeaseDeadline: now.Add(30 * time.Second), UserDeadline: &deadline, Generation: 1, ETag: `"ptv1:preview_lease:cHJ2X3B1Ymxpc2g:1"`}
			ownerDone, err := registry.OwnerSessionDoneForTarget("acct_cli", "machine_cli", owner, preview.OwnerSessionLocalLease, authority.lease.Target)
			if err != nil {
				t.Error(err)
			}
			authority.ownerDone = ownerDone
			lease := authority.lease
			host, err := preview.StartExisting(t.Context(), preview.SessionConfig{LeaseClient: authority, Carrier: authority, OwnerMachineID: lease.OwnerMachineID, OwnerSessionID: owner, OwnerSessionKind: lease.OwnerSessionKind, Target: lease.Target, AccessMode: lease.AccessMode, OwnerDone: ownerDone, DisableParentWatch: true}, lease)
			if err != nil {
				t.Error(err)
			}
			authority.mu.Lock()
			authority.host = host
			authority.mu.Unlock()
			return apiLeaseFromPreview(lease), lease.ETag, http.StatusOK
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/previews/prv_publish" {
			authority.mu.Lock()
			defer authority.mu.Unlock()
			return apiLeaseFromPreview(authority.lease), authority.lease.ETag, http.StatusOK
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		return nil, "", http.StatusNotFound
	})
	plainServer := apiServer
	apiServer = httptest.NewTLSServer(plainServer.Config.Handler)
	plainServer.Close()
	defer apiServer.Close()
	defer func() {
		authority.mu.Lock()
		host := authority.host
		authority.mu.Unlock()
		if host != nil {
			_ = host.Stop(context.Background())
		}
	}()
	auth := publicationMachineSource(t, apiServer.URL)
	carrier, err := preview.NewLeaseObserverCarrier(preview.LeaseObserverCarrierConfig{MachineAuth: auth})
	if err != nil {
		t.Fatal(err)
	}
	client := api.New(apiServer.URL, config.Credential{AccessToken: "client_token"}, apiServer.Client())
	oldClient, oldMachine, oldCarrier, oldOwner := previewClientForCommand, previewMachineID, newPreviewCarrier, previewOwnerSessionClientForCommand
	t.Cleanup(func() {
		previewClientForCommand, previewMachineID, newPreviewCarrier, previewOwnerSessionClientForCommand = oldClient, oldMachine, oldCarrier, oldOwner
	})
	previewClientForCommand = func(*cobra.Command) (*api.Client, error) { return client, nil }
	previewMachineID = func() (string, error) { return "machine_cli", nil }
	newPreviewCarrier = func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error) {
		return carrier, nil
	}
	previewOwnerSessionClientForCommand = func() (*preview.LocalOwnerSessionClient, error) { return ownerClient, nil }
	command := previewCobraCommandV1()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{origin.URL, "--background", "--ttl", "5m", "--json"})
	started := time.Now()
	err = command.ExecuteContext(t.Context())
	authority.mu.Lock()
	renewals, stops, lease := authority.renewals, authority.stops, authority.lease
	authority.mu.Unlock()
	if err != nil {
		t.Fatalf("legitimate pending publication stopped at %s: %v (renewals=%d stops=%d local_releases=%d)", time.Since(started), err, renewals, stops, ownerHandler.count(http.MethodDelete))
	}
	if renewals < 3 || stops != 0 || ownerHandler.count(http.MethodDelete) != 0 || lease.State != "ready" {
		t.Fatalf("publication lost ownership: renewals=%d stops=%d local_releases=%d state=%s", renewals, stops, ownerHandler.count(http.MethodDelete), lease.State)
	}
	select {
	case <-authority.ownerDone:
		t.Fatal("ready background owner was released")
	default:
	}
	if lease.UserDeadline == nil || time.Until(*lease.UserDeadline) > 5*time.Minute {
		t.Fatal("publication extended requested TTL")
	}
}

func publicationMachineSource(t *testing.T, controlURL string) *machinecontrol.Source {
	t.Helper()
	now := time.Now().UTC()
	root := filepath.Join(t.TempDir(), "identity")
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: controlURL, MachineID: "machine_cli", EnvironmentID: "environment_cli", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMachineControl(identity.MachineControl{MachineID: "machine_cli", EnvironmentID: "environment_cli", InstallationGeneration: 1, Credential: strings.Repeat("c", 40), ExpiresAt: now.Add(time.Hour), KeyID: key.ID}); err != nil {
		t.Fatal(err)
	}
	source, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: controlURL, StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

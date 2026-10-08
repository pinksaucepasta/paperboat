package environmentmanager

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type vaultHostControl struct {
	*vaultScopesControl
	host             api.VaultHostState
	requests         []api.VaultHostProvision
	failAfterCommit  bool
	conflictOnce     bool
	rotationRequired bool
	hostReadErr      error
}

func (c *vaultHostControl) GetVaultHost(context.Context, string) (api.VaultHostState, error) {
	return c.host, c.hostReadErr
}
func (c *vaultHostControl) PendingVaultHosts(context.Context) ([]api.VaultHostSummary, error) {
	return []api.VaultHostSummary{{MachineID: c.host.Bundle.MachineID, State: "pending", ProjectionRevision: c.host.Bundle.ProjectionRevision}}, nil
}
func (c *vaultHostControl) ProvisionVaultHost(_ context.Context, machine string, in api.VaultHostProvision) (environmente2ee.ProjectionBundle, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return environmente2ee.ProjectionBundle{}, err
	}
	defer local.Clear()
	var staged api.VaultHostProvision
	if local.Operation == nil || local.Operation.Kind != "host-provision" || json.Unmarshal(local.Operation.Request, &staged) != nil || staged.OperationID != in.OperationID || staged.Envelope != in.Envelope {
		return environmente2ee.ProjectionBundle{}, errors.New("host operation not durably staged")
	}
	c.requests = append(c.requests, in)
	if c.rotationRequired {
		return environmente2ee.ProjectionBundle{}, &api.APIError{Status: 409, Code: "rotation_required"}
	}
	if c.conflictOnce {
		c.conflictOnce = false
		return environmente2ee.ProjectionBundle{}, &api.APIError{Status: 409, Code: "version_conflict"}
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(in.Envelope)
	if err != nil {
		return environmente2ee.ProjectionBundle{}, err
	}
	projection, err := environmente2ee.ParseHostProjection(raw, c.writerPublic)
	if err != nil {
		return environmente2ee.ProjectionBundle{}, err
	}
	c.host.Selection = append([]api.VaultHostSelection{}, in.Selection...)
	c.host.Bundle.Envelope = in.Envelope
	c.host.Bundle.DocumentID = projection.ID.String()
	c.host.Bundle.ProjectionRevision = projection.Claims.Revision
	c.host.Bundle.SelectionGeneration = projection.Claims.SelectionGeneration
	c.host.Bundle.State = "ready"
	if c.failAfterCommit {
		c.failAfterCommit = false
		return environmente2ee.ProjectionBundle{}, errors.New("lost committed response")
	}
	return c.host.Bundle, nil
}
func newVaultHostFixture(t *testing.T) (PasswordVault, *vaultHostControl, *ecdh.PrivateKey) {
	t.Helper()
	v, c, _ := newVaultScopesFixture(t)
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &vaultHostControl{vaultScopesControl: c, host: api.VaultHostState{Bundle: environmente2ee.ProjectionBundle{AccountID: v.AccountID, MachineID: "machine_1", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), WriterPublic: base64.RawURLEncoding.EncodeToString(c.writerPublic), State: "pending"}}}
	v.Client = h
	return v, h, key
}
func hostValues(t *testing.T, c *vaultHostControl, key *ecdh.PrivateKey) map[string][]byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.Strict().DecodeString(c.host.Bundle.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	p, err := environmente2ee.ParseHostProjection(raw, c.writerPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := environmente2ee.OpenHostProjection(context.Background(), p, p.Claims, key.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return values
}
func TestVaultHostDeletedSelectionOmitsValueWithoutNewGrant(t *testing.T) {
	v, c, key := newVaultHostFixture(t)
	ctx := context.Background()
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(m map[string][]byte) error { m["SELECTED"] = []byte("private test value"); return nil }); err != nil {
		t.Fatal(err)
	}
	selection := []api.VaultHostSelection{{OwnerKind: "personal", OwnerID: v.AccountID, Name: "SELECTED"}}
	if err := v.ProvisionHost(ctx, "machine_1", selection); err != nil {
		t.Fatal(err)
	}
	values := hostValues(t, c, key)
	if !bytes.Equal(values["SELECTED"], []byte("private test value")) {
		t.Fatal("selected value missing")
	}
	clearVaultValues(values)
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(m map[string][]byte) error { clear(m["SELECTED"]); delete(m, "SELECTED"); return nil }); err != nil {
		t.Fatal(err)
	}
	if n, err := v.ReconcileHosts(ctx); err != nil || n != 1 {
		t.Fatalf("refresh count=%d error=%v", n, err)
	}
	values = hostValues(t, c, key)
	defer clearVaultValues(values)
	if len(values) != 0 || len(c.host.Selection) != 1 || c.host.Selection[0] != selection[0] {
		t.Fatal("deletion did not remove value while retaining authorization slot")
	}
	newSelection := append(append([]api.VaultHostSelection{}, selection...), api.VaultHostSelection{OwnerKind: "personal", OwnerID: v.AccountID, Name: "UNAUTHORIZED"})
	count := len(c.requests)
	if err := v.ProvisionHost(ctx, "machine_1", newSelection); !errors.Is(err, environmente2ee.ErrInvalid) || len(c.requests) != count {
		t.Fatal("new missing name was granted")
	}
}
func TestVaultHostRefreshConflictAndAmbiguousRetry(t *testing.T) {
	v, c, _ := newVaultHostFixture(t)
	ctx := context.Background()
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(m map[string][]byte) error { m["A"] = []byte("a"); return nil }); err != nil {
		t.Fatal(err)
	}
	selection := []api.VaultHostSelection{{OwnerKind: "personal", OwnerID: v.AccountID, Name: "A"}}
	if err := v.ProvisionHost(ctx, "machine_1", selection); err != nil {
		t.Fatal(err)
	}
	c.conflictOnce = true
	if n, err := v.ReconcileHosts(ctx); err != nil || n != 1 {
		t.Fatalf("conflict refresh=%d %v", n, err)
	}
	last := len(c.requests) - 1
	if c.requests[last].OperationID == c.requests[last-1].OperationID {
		t.Fatal("definitely rejected operation was not rebuilt")
	}
	c.failAfterCommit = true
	if _, err := v.ReconcileHosts(ctx); err == nil {
		t.Fatal("ambiguous result hidden")
	}
	failed := c.requests[len(c.requests)-1]
	if _, err := v.ReconcileHosts(ctx); !errors.Is(err, ErrVaultPending) {
		t.Fatal("pending ciphertext replaced")
	}
	if err := v.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	retried := c.requests[len(c.requests)-1]
	if failed.OperationID != retried.OperationID || failed.Envelope != retried.Envelope {
		t.Fatal("ambiguous retry changed exact operation")
	}
	if err := v.Store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ReconcileHosts(ctx); !errors.Is(err, ErrVaultLocked) {
		t.Fatal("locked vault refreshed")
	}
}

func TestVaultHostRotationRejectionClearsOnlyVerifiedHostJournal(t *testing.T) {
	for _, scenario := range []string{"unpublished", "published", "retired", "ambiguous-read"} {
		t.Run(scenario, func(t *testing.T) {
			v, c, _ := newVaultHostFixture(t)
			ctx := context.Background()
			if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(m map[string][]byte) error { m["A"] = []byte("a"); return nil }); err != nil {
				t.Fatal(err)
			}
			selection := []api.VaultHostSelection{{OwnerKind: "personal", OwnerID: v.AccountID, Name: "A"}}
			if err := v.ProvisionHost(ctx, "machine_1", selection); err != nil {
				t.Fatal(err)
			}
			if scenario == "unpublished" {
				c.rotationRequired = true
				err := v.ProvisionHost(ctx, "machine_1", selection)
				var rotation *HostRotationRequired
				if !errors.As(err, &rotation) || rotation.Published {
					t.Fatal("rejected publication lost rotation recovery")
				}
			} else {
				c.failAfterCommit = true
				if err := v.ProvisionHost(ctx, "machine_1", selection); err == nil {
					t.Fatal("lost result hidden")
				}
				c.rotationRequired = true
				if scenario == "retired" {
					c.hostReadErr = &api.APIError{Status: 404}
				}
				if scenario == "ambiguous-read" {
					c.hostReadErr = errors.New("private transient read")
				}
				err := v.Resume(ctx)
				if scenario == "ambiguous-read" {
					var pending *HostPublicationPending
					if !errors.As(err, &pending) {
						t.Fatal("ambiguous host read discarded exact retry")
					}
				} else {
					var rotation *HostRotationRequired
					if !errors.As(err, &rotation) || (scenario == "published" && !rotation.Published) {
						t.Fatal("published/retired cursor was not reconciled")
					}
				}
			}
			local, err := v.Store.LoadPasswordVault(v.Issuer, v.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			defer local.Clear()
			if (local.Operation != nil) != (scenario == "ambiguous-read") {
				t.Fatal("incorrect staged host operation retention")
			}
			if local.Pending != nil || len(local.Payload) == 0 {
				t.Fatal("rotation recovery damaged vault custody")
			}
		})
	}
}

func TestVaultHostConflictRetryDoesNotHideIndependentFailure(t *testing.T) {
	conflict := &HostRefreshConflict{Cause: &api.APIError{Status: 409, Code: "version_conflict"}}
	if !hostRefreshCanRetry(errors.Join(conflict, nil)) {
		t.Fatal("single rejected publication lost retry")
	}
	if hostRefreshCanRetry(errors.Join(conflict, errors.New("independent cleanup failure"))) {
		t.Fatal("retry hid an independent cleanup failure")
	}
}

func TestVaultHostInitialRegisteredEmptySelectionPublishesWithoutExpandingGrant(t *testing.T) {
	v, c, key := newVaultHostFixture(t)
	if c.host.Bundle.ProjectionRevision != 0 || len(c.host.Selection) != 0 {
		t.Fatal("fixture is not an unprovisioned registered owner host")
	}
	if n, err := v.ReconcileHosts(context.Background()); err != nil || n != 1 {
		t.Fatalf("initial refresh count=%d error=%v", n, err)
	}
	values := hostValues(t, c, key)
	defer clearVaultValues(values)
	if len(values) != 0 || len(c.host.Selection) != 0 || c.host.Bundle.ProjectionRevision != 1 {
		t.Fatal("initial delivery expanded an empty selection")
	}
}

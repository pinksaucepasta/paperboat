//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestLayerEnvironmentLaunchRequiresFreshProofBoundWorkspaceActor(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	registration := layerRegistration(t)
	registration.AccountID = "account_1"
	root := filepath.Join(t.TempDir(), "runtime")
	source, err := productionEnvironmentKeySourceForState(root, registration)
	if err != nil {
		t.Fatal(err)
	}
	material, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	credentials := &layerTestCredentials{token: "test-token", proof: []byte("machine-proof")}
	calls := 0
	foreign := false
	unavailable := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environment/hosts/machine_1/key" {
			writeLayerRecipientRegistrationResponse(t, w, r, credentials, registration, material.Generation, public[:])
			return
		}
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		operation, method, path, signed, _ := credentials.snapshot()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/environment/hosts/machine_1/layers" || method != r.Method || path != r.URL.Path || !bytes.Equal(body, signed) || r.Header.Get("Idempotency-Key") != operation || r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("X-Paperboat-Machine-Proof") != base64.RawURLEncoding.EncodeToString(credentials.proof) {
			t.Error("launch context not exact proof-bound")
		}
		var binding struct {
			OperationID    string `json:"operation_id"`
			WorkspaceID    string `json:"workspace_id"`
			ActorAccountID string `json:"actor_account_id"`
		}
		if json.Unmarshal(body, &binding) != nil || binding.OperationID != operation || binding.WorkspaceID != "team_1" || binding.ActorAccountID != "account_1" {
			t.Error("wrong launch actor/workspace")
		}
		if unavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		bundle := api.VaultLayerContext{WorkspaceID: binding.WorkspaceID, ActorAccountID: binding.ActorAccountID, MachineID: registration.MachineID, Layers: []api.VaultLayerDelivery{}, Recipient: api.VaultLayerRecipient{RecipientAccount: registration.AccountID, MachineID: registration.MachineID, InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: base64.RawURLEncoding.EncodeToString(public[:])}}
		if foreign {
			bundle.ActorAccountID = "account_2"
		}
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"data": bundle})
	}))
	defer server.Close()
	service := newLayerEnvironmentService(root, layerServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	if _, err := service.EnvironmentForLaunch(context.Background()); err == nil || calls != 0 {
		t.Fatal("unverified actor launched")
	}
	ctx := envinject.WithLaunchContext(context.Background(), "team_1", "account_1")
	for i := 0; i < 2; i++ {
		values, err := service.EnvironmentForLaunch(ctx)
		if err != nil || len(values) != 0 {
			t.Fatal("fresh empty context failed")
		}
	}
	if calls != 2 {
		t.Fatal("authorization context cached across launches")
	}
	foreign = true
	if _, err := service.EnvironmentForLaunch(ctx); err == nil {
		t.Fatal("foreign actor accepted")
	}
	foreign = false
	unavailable = true
	if _, err := service.EnvironmentForLaunch(ctx); err == nil {
		t.Fatal("outage silently reused cached ENV")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := calls
	if _, err := service.EnvironmentForLaunch(cancelled); err == nil || calls != before {
		t.Fatal("cancelled launch requested ENV")
	}
}

func TestLayerEnvironmentObservationLostAckRetainsDurableExactReport(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	registration := layerRegistration(t)
	registration.AccountID = "account_1"
	root := filepath.Join(t.TempDir(), "runtime")
	source, err := productionEnvironmentKeySourceForState(root, registration)
	if err != nil {
		t.Fatal(err)
	}
	material, err := source.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	credentials := &layerTestCredentials{token: "test-token", proof: []byte("proof")}
	var delivery api.VaultLayerDelivery
	sends := 0
	accept := false
	var first api.VaultLayerObservation
	var firstOperation string
	var firstBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environment/hosts/machine_1/key" {
			writeLayerRecipientRegistrationResponse(t, w, r, credentials, registration, material.Generation, public[:])
			return
		}
		if r.URL.Path == "/v1/environment/hosts/machine_1/layers" {
			w.Header().Set("Cache-Control", "no-store")
			json.NewEncoder(w).Encode(map[string]any{"data": api.VaultLayerContext{WorkspaceID: "personal", ActorAccountID: "account_1", MachineID: "machine_1", Recipient: delivery.Recipient, Layers: []api.VaultLayerDelivery{delivery}}})
			return
		}
		if r.URL.Path != "/v1/environment/hosts/machine_1/layer-observations" {
			t.Error("unexpected report path")
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		operation, method, path, signed, _ := credentials.snapshot()
		if method != http.MethodPost || path != r.URL.Path || !bytes.Equal(body, signed) || operation != r.Header.Get("Idempotency-Key") {
			t.Error("observation not exact proof-bound")
		}
		var batch struct {
			OperationID  string                      `json:"operation_id"`
			Observations []api.VaultLayerObservation `json:"observations"`
		}
		if json.Unmarshal(body, &batch) != nil || batch.OperationID != operation || len(batch.Observations) != 1 {
			t.Error("invalid report batch")
			w.WriteHeader(400)
			return
		}
		report := batch.Observations[0]
		sends++
		if sends == 1 {
			first = report
			firstOperation = operation
			firstBody = bytes.Clone(body)
		} else if report != first || operation != firstOperation || !bytes.Equal(body, firstBody) {
			t.Error("retry changed durable report")
		}
		if !accept {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"operation_id": operation, "accepted": true}})
	}))
	defer server.Close()
	writer := bytes.Repeat([]byte{0x49}, 32)
	defer clear(writer)
	sourceID := env.DocumentID([32]byte{1})
	claims := env.VaultLayerClaims{Issuer: server.URL, RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: public[:], DeliveryGeneration: 1, Previous: make([]byte, 32), FenceGeneration: 1, Source: env.VaultLayerSource{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1", KeyEpoch: 1, Revision: 1, Digest: sourceID[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}
	layer, err := env.SealVaultLayer(context.Background(), claims, writer, map[string][]byte{"VALUE": []byte("test-value")})
	if err != nil {
		t.Fatal(err)
	}
	delivery = api.VaultLayerDelivery{Recipient: api.VaultLayerRecipient{RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: base64.RawURLEncoding.EncodeToString(public[:]), DeliveryGeneration: 1, DocumentID: layer.ID.String(), FenceGeneration: 1}, Source: api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "account_1"}, KeyEpoch: 1, Revision: 1, DocumentID: sourceID.String()}, WriterAccount: "account_1", WriterPublic: base64.RawURLEncoding.EncodeToString(layer.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(layer.Raw), State: "ready"}
	service := newLayerEnvironmentService(root, layerServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	ctx := envinject.WithLaunchContext(context.Background(), "personal", "account_1")
	values, err := service.EnvironmentForLaunch(ctx)
	if err != nil || len(values) != 1 {
		t.Fatal("report outage prevented already verified launch")
	}
	pending, err := service.layers.PendingObservations(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal("failed report lost durable retry")
	}
	accept = true
	if err := service.FlushLayerObservations(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err = service.layers.PendingObservations(ctx)
	if err != nil || len(pending) != 0 || sends != 2 {
		t.Fatal("matching ACK did not clear report")
	}
}

func writeLayerRecipientRegistrationResponse(t *testing.T, w http.ResponseWriter, r *http.Request, credentials *layerTestCredentials, registration runtimeidentity.Registration, generation uint64, public []byte) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	operation, method, path, signed, _ := credentials.snapshot()
	if method != http.MethodPost || r.Method != method || path != r.URL.Path || !bytes.Equal(raw, signed) || r.Header.Get("Idempotency-Key") != operation {
		t.Error("recipient registration not proof-bound")
	}
	var request struct {
		OperationID            string `json:"operation_id"`
		InstallationGeneration uint64 `json:"installation_generation"`
		HostKeyGeneration      uint64 `json:"host_key_generation"`
		HostPublic             string `json:"host_public"`
	}
	if json.Unmarshal(raw, &request) != nil || request.OperationID != operation || request.InstallationGeneration != uint64(registration.InstallationGeneration) || request.HostKeyGeneration != generation || request.HostPublic != base64.RawURLEncoding.EncodeToString(public) {
		t.Error("wrong recipient registration binding")
	}
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"data": api.VaultLayerRecipient{RecipientAccount: registration.AccountID, MachineID: registration.MachineID, InstallationGeneration: request.InstallationGeneration, HostKeyGeneration: request.HostKeyGeneration, HostPublic: request.HostPublic}})
}

type layerTestCredentials struct {
	mu         sync.Mutex
	token      string
	proof      []byte
	operation  string
	method     string
	path       string
	body       []byte
	calls      int
	started    chan struct{}
	startedOne sync.Once
}

func (c *layerTestCredentials) Token(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

func (c *layerTestCredentials) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	c.mu.Lock()
	c.operation = operationID
	c.method = method
	c.path = path
	c.body = bytes.Clone(body)
	c.calls++
	if c.started != nil {
		c.startedOne.Do(func() { close(c.started) })
	}
	proof := bytes.Clone(c.proof)
	c.mu.Unlock()
	return proof, nil
}

func (c *layerTestCredentials) snapshot() (operation, method, path string, body []byte, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operation, c.method, c.path, bytes.Clone(c.body), c.calls
}

func layerRegistration(t *testing.T) runtimeidentity.Registration {
	t.Helper()
	return runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 11}
}

func layerServiceURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestLayerObservationFlushTraverses129ContextsAndKeepsRejectedWorkspace(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	ctx := context.Background()
	registration := layerRegistration(t)
	registration.AccountID = "account_1"
	root := filepath.Join(t.TempDir(), "runtime")
	keys, err := productionEnvironmentKeySourceForState(root, registration)
	if err != nil {
		t.Fatal(err)
	}
	material, err := keys.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := keys.(environmentkey.LayerGenesisMarker)
	if !ok {
		t.Fatal("missing layer marker")
	}
	store, err := envinject.NewLayerStore(envinject.LayerConfig{Path: filepath.Join(root, "layers.json"), Issuer: "https://control.example", AccountID: "account_1", MachineID: "machine_1", InstallationGeneration: uint64(registration.InstallationGeneration), Keys: keys, Marker: marker})
	if err != nil {
		t.Fatal(err)
	}
	writer := bytes.Repeat([]byte{0x49}, 32)
	defer clear(writer)
	for i := 0; i < 129; i++ {
		workspace := fmt.Sprintf("team_%03d", i)
		id := env.DocumentID([32]byte{1})
		claims := env.VaultLayerClaims{Issuer: "https://control.example", RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: public[:], DeliveryGeneration: 1, Previous: make([]byte, 32), FenceGeneration: 1, Source: env.VaultLayerSource{WorkspaceID: workspace, OwnerKind: "team", OwnerID: workspace, KeyEpoch: 1, Revision: 1, Digest: id[:]}, WriterAccount: "account_1", WriterVaultGeneration: 1}
		layer, err := env.SealVaultLayer(ctx, claims, writer, map[string][]byte{"VALUE": []byte("fixture")})
		if err != nil {
			t.Fatal(err)
		}
		recipient := api.VaultLayerRecipient{RecipientAccount: "account_1", MachineID: "machine_1", InstallationGeneration: uint64(registration.InstallationGeneration), HostKeyGeneration: material.Generation, HostPublic: base64.RawURLEncoding.EncodeToString(public[:]), DeliveryGeneration: 1, DocumentID: layer.ID.String(), FenceGeneration: 1}
		delivery := api.VaultLayerDelivery{Recipient: recipient, Source: api.VaultLayerSource{VaultLayerCoordinate: api.VaultLayerCoordinate{WorkspaceID: workspace, OwnerKind: "team", OwnerID: workspace}, KeyEpoch: 1, Revision: 1, DocumentID: id.String()}, WriterAccount: "account_1", WriterPublic: base64.RawURLEncoding.EncodeToString(layer.Claims.WriterPublic), Envelope: base64.RawURLEncoding.EncodeToString(layer.Raw), State: "ready"}
		if _, err := store.Environment(ctx, envinject.LaunchContext{WorkspaceID: workspace, ActorAccountID: "account_1"}, api.VaultLayerContext{Recipient: recipient, WorkspaceID: workspace, ActorAccountID: "account_1", MachineID: "machine_1", Layers: []api.VaultLayerDelivery{delivery}}); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := store.PendingObservations(ctx)
	if err != nil || len(pending) != 129 {
		t.Fatal("pending reports silently truncated")
	}
	credentials := &layerTestCredentials{token: "fixture-token", proof: []byte("fixture-proof")}
	calls := 0
	accepted := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var batch struct {
			OperationID  string                      `json:"operation_id"`
			Observations []api.VaultLayerObservation `json:"observations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil || len(batch.Observations) != 1 {
			t.Error("mixed workspace or invalid report batch")
			w.WriteHeader(400)
			return
		}
		if batch.Observations[0].WorkspaceID == "team_000" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		accepted[batch.Observations[0].WorkspaceID]++
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"operation_id": batch.OperationID, "accepted": true}})
	}))
	defer server.Close()
	service := newLayerEnvironmentService(root, layerServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	service.layers = store
	for {
		before := len(pending)
		if err := service.FlushLayerObservations(ctx); err == nil {
			t.Fatal("revoked workspace report failure hidden")
		}
		pending, err = store.PendingObservations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 1 {
			break
		}
		if len(pending) >= before {
			t.Fatal("bounded report flush made no progress on authorized workspaces")
		}
	}
	if calls < 129 || len(accepted) != 128 || pending[0].WorkspaceID != "team_000" {
		t.Fatalf("report completion count=%d acknowledged=%d pending=%d", calls, len(accepted), len(pending))
	}
	for _, count := range accepted {
		if count != 1 {
			t.Fatal("durably acknowledged report was retransmitted")
		}
	}
}

//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	peeridentityenrollment "github.com/pinksaucepasta/paperboat/internal/hostruntime/peeridentity"
	"golang.org/x/crypto/ssh"
)

type testTokenSource struct{}

type runtimeObservationConnector struct{}

type peerEnrollmentSequence struct {
	errors []error
	calls  int
}

func (s *peerEnrollmentSequence) Ensure(context.Context) error {
	s.calls++
	if len(s.errors) == 0 {
		return nil
	}
	err := s.errors[0]
	s.errors = s.errors[1:]
	return err
}

func (runtimeObservationConnector) Status() connector.Status {
	return connector.Status{Connected: true, Generation: 2}
}

func (testTokenSource) Token(context.Context) (string, error) { return "helper-identity", nil }

type testProofSource struct{ body []byte }

func (p *testProofSource) Proof(_ context.Context, _ string, method, path string, body []byte) ([]byte, error) {
	if method != http.MethodPost || path != "/v1/runtime-observations" {
		return nil, errors.New("wrong proof target")
	}
	p.body = append([]byte(nil), body...)
	return []byte("proof"), nil
}

type managedSSHTestIdentity struct {
	tokens []string
	proofs []managedSSHTestProof
}

type managedSSHTestProof struct {
	operationID string
	method      string
	path        string
	body        []byte
}

func (s *managedSSHTestIdentity) Token(context.Context) (string, error) {
	if len(s.tokens) == 0 {
		return "", errors.New("no managed SSH identity")
	}
	token := s.tokens[0]
	s.tokens = s.tokens[1:]
	return token, nil
}

func (s *managedSSHTestIdentity) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	s.proofs = append(s.proofs, managedSSHTestProof{operationID: operationID, method: method, path: path, body: append([]byte(nil), body...)})
	return []byte("proof-" + operationID), nil
}

type managedSSHTestClient struct {
	observedIdentity string
	observedProof    []byte
	keysIdentity     string
	keysProof        []byte
}

func (c *managedSSHTestClient) ObserveManagedSSHHostKeys(_ context.Context, machineID, identity, operationID string, generation, observation uint64, keys []string, proof []byte) (clientapi.ManagedSSHHostKeySet, error) {
	wantFingerprint := sha256.Sum256([]byte("host-set-one"))
	wantObserveOperation, _ := managedSSHInitialOperationIDs(runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, 7, wantFingerprint)
	if machineID != "machine_1" || operationID != wantObserveOperation || generation != 4 || observation != 7 || len(keys) != 1 {
		return clientapi.ManagedSSHHostKeySet{}, errors.New("wrong host-key observation")
	}
	c.observedIdentity, c.observedProof = identity, append([]byte(nil), proof...)
	return clientapi.ManagedSSHHostKeySet{State: "active"}, nil
}

func (c *managedSSHTestClient) ManagedSSHAuthorizedKeys(_ context.Context, machineID, identity string, generation uint64, proof []byte) (clientapi.ManagedSSHAuthorizedKeys, error) {
	if machineID != "machine_1" || generation != 4 {
		return clientapi.ManagedSSHAuthorizedKeys{}, errors.New("wrong authorized-key request")
	}
	c.keysIdentity, c.keysProof = identity, append([]byte(nil), proof...)
	return clientapi.ManagedSSHAuthorizedKeys{Keys: []string{"ssh-ed25519 AAAA managed"}}, nil
}

func TestManagedSSHAuthorityUsesCurrentCredentialAndExactProofBodies(t *testing.T) {
	identity := &managedSSHTestIdentity{tokens: []string{"machine-credential-1", "machine-credential-2"}}
	client := &managedSSHTestClient{}
	registration := runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}
	fingerprint := sha256.Sum256([]byte("host-set-one"))
	observeOperationID, keyOperationID := managedSSHInitialOperationIDs(registration, 7, fingerprint)
	keys, active, err := reconcileManagedSSHAuthorityWithFingerprint(t.Context(), client, identity, registration, 7, fingerprint, []string{"ssh-ed25519 AAAA host"})
	if err != nil || !active || len(keys.Keys) != 1 {
		t.Fatalf("keys=%#v active=%t err=%v", keys, active, err)
	}
	if client.observedIdentity != "machine-credential-1" || client.keysIdentity != "machine-credential-2" || len(identity.proofs) != 2 {
		t.Fatalf("identities=%q,%q proofs=%d", client.observedIdentity, client.keysIdentity, len(identity.proofs))
	}
	var observed struct {
		ObservationGeneration uint64   `json:"observation_generation"`
		PublicKeys            []string `json:"public_keys"`
	}
	if json.Unmarshal(identity.proofs[0].body, &observed) != nil || observed.ObservationGeneration != 7 || len(observed.PublicKeys) != 1 || identity.proofs[0].method != http.MethodPut || identity.proofs[0].path != "/v1/machines/machine_1/ssh-host-keys" {
		t.Fatalf("observation proof = %#v body=%s", identity.proofs[0], identity.proofs[0].body)
	}
	if string(identity.proofs[1].body) != "{}" || identity.proofs[1].method != http.MethodPost || identity.proofs[1].path != "/v1/machines/machine_1/ssh-authorized-keys" {
		t.Fatalf("authorized-key proof = %#v", identity.proofs[1])
	}
	if string(client.observedProof) != "proof-"+observeOperationID || string(client.keysProof) != "proof-"+keyOperationID {
		t.Fatalf("proofs=%q,%q", client.observedProof, client.keysProof)
	}
}

func TestManagedSSHInitialOperationIDsBindExactHostKeyFingerprint(t *testing.T) {
	registration := runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}
	first := sha256.Sum256([]byte("host-set-one"))
	second := sha256.Sum256([]byte("host-set-two"))
	observeFirst, keysFirst := managedSSHInitialOperationIDs(registration, 7, first)
	observeReplay, keysReplay := managedSSHInitialOperationIDs(registration, 7, first)
	observeSecond, keysSecond := managedSSHInitialOperationIDs(registration, 7, second)
	observeLater, keysLater := managedSSHInitialOperationIDs(registration, 8, first)
	if observeFirst != observeReplay || keysFirst != keysReplay {
		t.Fatalf("same fingerprint did not produce stable operation IDs: %q/%q vs %q/%q", observeFirst, keysFirst, observeReplay, keysReplay)
	}
	if observeFirst == observeSecond || keysFirst == keysSecond {
		t.Fatalf("different fingerprints reused operation IDs: %q/%q vs %q/%q", observeFirst, keysFirst, observeSecond, keysSecond)
	}
	if observeFirst == observeLater || keysFirst == keysLater {
		t.Fatalf("different observation generations reused operation IDs: %q/%q vs %q/%q", observeFirst, keysFirst, observeLater, keysLater)
	}
	registration.InstallationGeneration++
	observeReplacement, keysReplacement := managedSSHInitialOperationIDs(registration, 7, first)
	if observeReplacement == observeFirst || keysReplacement == keysFirst {
		t.Fatal("replacement installation reused prior observation identity")
	}
	if len(observeFirst) > 128 || len(keysFirst) > 128 {
		t.Fatalf("operation IDs exceed machine-proof bound: %d/%d", len(observeFirst), len(keysFirst))
	}
}

type rotatingManagedSSHClient struct {
	mu            sync.Mutex
	keys          [][]string
	calls         int
	hostStates    []string
	observeErrors []error
	hostCalls     int
}

func (c *rotatingManagedSSHClient) ObserveManagedSSHHostKeys(context.Context, string, string, string, uint64, uint64, []string, []byte) (clientapi.ManagedSSHHostKeySet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.hostCalls
	c.hostCalls++
	if index < len(c.observeErrors) && c.observeErrors[index] != nil {
		return clientapi.ManagedSSHHostKeySet{}, c.observeErrors[index]
	}
	state := "active"
	if len(c.hostStates) > 0 {
		if index >= len(c.hostStates) {
			index = len(c.hostStates) - 1
		}
		state = c.hostStates[index]
	}
	return clientapi.ManagedSSHHostKeySet{State: state}, nil
}

func managedSSHTestPublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPublic)))
}

func managedSSHRuntimeTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func (c *rotatingManagedSSHClient) ManagedSSHAuthorizedKeys(context.Context, string, string, uint64, []byte) (clientapi.ManagedSSHAuthorizedKeys, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	index := c.calls
	if index >= len(c.keys) {
		index = len(c.keys) - 1
	}
	c.calls++
	return clientapi.ManagedSSHAuthorizedKeys{Keys: append([]string(nil), c.keys[index]...)}, nil
}

type refreshingManagedSSHIdentity struct {
	mu         sync.Mutex
	operations []string
}

func (s *refreshingManagedSSHIdentity) Token(context.Context) (string, error) {
	return "machine-credential", nil
}
func (s *refreshingManagedSSHIdentity) Proof(_ context.Context, operationID, _, _ string, _ []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operations = append(s.operations, operationID)
	return []byte("proof-" + operationID), nil
}

func TestManagedSSHKeyReconcilerConvergesAddAndRevocation(t *testing.T) {
	key := managedSSHTestPublicKey(t)
	home := managedSSHRuntimeTestHome(t)
	client := &rotatingManagedSSHClient{keys: [][]string{{key}, nil}}
	identity := &refreshingManagedSSHIdentity{}
	service := &managedSSHKeyReconciler{client: client, identity: identity, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, publicKeys: []string{"ssh-ed25519 AAAA host"}, home: home, ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	path := filepath.Join(home, ".ssh", "authorized_keys")
	added, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(added, []byte(key)) {
		t.Fatalf("initial authorized_keys=%q error=%v", added, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		updated, readErr := os.ReadFile(path)
		if readErr == nil && !bytes.Contains(updated, []byte(key)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed key was not revoked: %q error=%v", updated, readErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	identity.mu.Lock()
	operations := append([]string(nil), identity.operations...)
	identity.mu.Unlock()
	if len(operations) < 4 || operations[0] == operations[2] || !strings.Contains(operations[0], "-machine_1-4-9-refresh-") {
		t.Fatalf("proof operations=%v", operations)
	}
}

func TestManagedSSHKeyReconcilerActivatesPromotedHostWithoutRestart(t *testing.T) {
	key := managedSSHTestPublicKey(t)
	home := managedSSHRuntimeTestHome(t)
	client := &rotatingManagedSSHClient{hostStates: []string{"pending", "active"}, keys: [][]string{{key}}}
	identity := &refreshingManagedSSHIdentity{}
	service := &managedSSHKeyReconciler{client: client, identity: identity, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 10, publicKeys: []string{"ssh-ed25519 AAAA host"}, home: home, ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	path := filepath.Join(home, ".ssh", "authorized_keys")
	initial, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) || bytes.Contains(initial, []byte(key)) {
		t.Fatalf("pending authorized_keys=%q error=%v", initial, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		updated, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(updated, []byte(key)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("promoted host did not activate managed key: %q error=%v", updated, readErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagedSSHKeyReconcilerFailsClosedWhenAuthorityRefreshFails(t *testing.T) {
	key := managedSSHTestPublicKey(t)
	home := managedSSHRuntimeTestHome(t)
	client := &rotatingManagedSSHClient{keys: [][]string{{key}}, observeErrors: []error{nil, errors.New("authority unavailable")}}
	service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 11, publicKeys: []string{"ssh-ed25519 AAAA host"}, home: home, ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	path := filepath.Join(home, ".ssh", "authorized_keys")
	deadline := time.Now().Add(time.Second)
	for {
		updated, readErr := os.ReadFile(path)
		if readErr == nil && !bytes.Contains(updated, []byte(key)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed key survived failed authority refresh: %q error=%v", updated, readErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestManagedSSHKeyReconcilerReportsTypedInitialAuthorityFailure(t *testing.T) {
	service := &managedSSHKeyReconciler{client: &rotatingManagedSSHClient{observeErrors: []error{errors.New("authority unavailable")}}, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 13, publicKeys: []string{"ssh-ed25519 AAAA host"}, home: t.TempDir(), ownerUID: uint32(os.Getuid()), interval: time.Hour, timeout: time.Second}
	if err := service.Start(t.Context()); !errors.Is(err, ErrManagedSSHUnavailable) {
		t.Fatalf("Start error=%v, want managed SSH unavailable", err)
	}
}

func TestManagedSSHKeyReconcilerRemovesManagedKeysOnShutdown(t *testing.T) {
	key := managedSSHTestPublicKey(t)
	home := managedSSHRuntimeTestHome(t)
	service := &managedSSHKeyReconciler{client: &rotatingManagedSSHClient{keys: [][]string{{key}}}, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 12, publicKeys: []string{"ssh-ed25519 AAAA host"}, home: home, ownerUID: uint32(os.Getuid()), interval: time.Hour, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	if installed, err := os.ReadFile(path); err != nil || !bytes.Contains(installed, []byte(key)) {
		t.Fatalf("initial authorized_keys=%q error=%v", installed, err)
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	removed, err := os.ReadFile(path)
	if err != nil || bytes.Contains(removed, []byte(key)) {
		t.Fatalf("managed key remained after shutdown: %q error=%v", removed, err)
	}
}

func TestRuntimeObservationUsesRenewableIdentityAndExactBodyProof(t *testing.T) {
	var gotAuth, gotProof string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		gotAuth, gotProof = r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Machine-Proof")
		if r.URL.Path != "/v1/runtime-observations" || !strings.Contains(body.String(), `"environment_id":"prj_1"`) {
			t.Errorf("request path/body = %s %s", r.URL.Path, body.String())
		}
		if strings.Contains(body.String(), `"environment":`) || strings.Contains(body.String(), `"environment_injection"`) {
			t.Errorf("unattached secure ENV provider advertised capability or observation")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	proofs := &testProofSource{}
	sender := &runtimeObservationSender{endpoint: server.URL + "/v1/runtime-observations", tokens: testTokenSource{}, proofs: proofs, operationID: func() (string, error) { return "op-1", nil }, environmentID: "prj_1", machineID: "mach_1", reporterVersion: "test", client: server.Client(), environment: &envinject.Provider{}, workerGeneration: 1, osBootID: "boot-1", serviceScope: "system", connector: runtimeObservationConnector{}}
	if err := sender.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer helper-identity" || gotProof != base64.RawURLEncoding.EncodeToString([]byte("proof")) {
		t.Fatalf("headers auth=%q proof=%q", gotAuth, gotProof)
	}
	if len(proofs.body) == 0 || !bytes.Contains(proofs.body, []byte(`"resource_id":"mach_1"`)) {
		t.Fatalf("proof body=%s", proofs.body)
	}
}

func TestEnvironmentInjectionRequiresEnrollment(t *testing.T) {
	registration := runtimeidentity.Registration{MachineID: "mach_1", InstallationGeneration: 1}
	if !environmentInjectionEligible(registration) {
		t.Fatal("machine host runtime did not enable ENV local key custody")
	}
	registration.InstallationGeneration = 0
	if environmentInjectionEligible(registration) {
		t.Fatal("unenrolled machine enabled ENV custody")
	}
}

type runtimeTestEnvironmentProcessor struct {
	variables map[string]string
}

func (p runtimeTestEnvironmentProcessor) Restore(context.Context, envinject.Cache) (envinject.Verified, error) {
	return envinject.Verified{}, nil
}

func (p runtimeTestEnvironmentProcessor) Apply(_ context.Context, cache envinject.Cache, _ envinject.Bundle) (envinject.Cache, envinject.Verified, error) {
	cache.Authority = &envinject.Cursor{Generation: 9, AuthorityID: "sha256:" + strings.Repeat("a", 64)}
	cache.GlobalManifest = &envinject.ManifestEnvelope{Version: 7, KeyEpoch: 2, ManifestID: "sha256:" + strings.Repeat("b", 64), Envelope: "ciphertext-global"}
	cache.MachineManifest = &envinject.ManifestEnvelope{Version: 4, KeyEpoch: 1, ManifestID: "sha256:" + strings.Repeat("c", 64), Envelope: "ciphertext-machine"}
	return cache, envinject.Verified{
		Authority: cache.Authority,
		Global:    &envinject.ManifestCursor{Version: 7, KeyEpoch: 2, ManifestID: cache.GlobalManifest.ManifestID},
		Machine:   &envinject.ManifestCursor{Version: 4, KeyEpoch: 1, ManifestID: cache.MachineManifest.ManifestID},
		Variables: p.variables,
		Ready:     true,
	}, nil
}

func TestRuntimeObservationAppliesEncryptedBundleAndAcknowledgesIt(t *testing.T) {
	variables := map[string]string{"GLOBAL_TOKEN": "global-secret", "MACHINE_TOKEN": "machine-secret"}
	var observations []envinject.Observation
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("global-secret")) || bytes.Contains(body, []byte("machine-secret")) {
			t.Fatal("plaintext environment value reached the runtime request")
		}
		var request struct {
			Environment        envinject.Observation         `json:"environment"`
			RuntimeDiagnostics runtimeDiagnosticsObservation `json:"runtime_diagnostics"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		requestCount++
		observations = append(observations, request.Environment)
		if requestCount == 1 && slices.Contains(request.RuntimeDiagnostics.Capabilities, "environment_injection") {
			t.Fatal("unverified ENV binding advertised environment_injection")
		}
		if requestCount > 1 && !slices.Contains(request.RuntimeDiagnostics.Capabilities, "environment_injection") {
			t.Fatal("verified ENV binding omitted environment_injection")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"accepted": true,
			"environment_bundle": envinject.Bundle{
				Schema: envinject.BundleSchema,
			},
		}})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "environment-cache.json")
	store, err := envinject.Open(context.Background(), envinject.Config{
		Path: path, HighWaterPath: filepath.Join(t.TempDir(), "environment-high-water.json"), IntegrityKey: bytes.Repeat([]byte{0x42}, 32), AllowHighWaterInitialize: true, AccountID: "acct_1", MachineID: "mach_1",
		InstallationGeneration: 1, HostKeyGeneration: 1, HostRecipientKeyID: runtimeTestHostRecipientKeyID,
		GenesisMarker: runtimeTestGenesisMarkerFor(t, path),
		Processor:     runtimeTestEnvironmentProcessor{variables: variables},
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &runtimeObservationSender{endpoint: server.URL, tokens: testTokenSource{}, proofs: &testProofSource{}, operationID: func() (string, error) { return "op-1", nil }, environmentID: "prj_1", machineID: "mach_1", reporterVersion: "test", client: server.Client(), environment: store, workerGeneration: 1, osBootID: "boot-1", serviceScope: "system", connector: runtimeObservationConnector{}}
	if err := sender.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].State != "pending" || observations[1].State != "applied" || observations[1].Global == nil || observations[1].Global.Version != 7 || observations[1].Machine == nil || observations[1].Machine.Version != 4 || observations[1].ObservationSeq != 2 {
		t.Fatalf("observations=%+v", observations)
	}
	if got, err := store.Environment(); err != nil || !reflect.DeepEqual(got, []string{"GLOBAL_TOKEN=global-secret", "MACHINE_TOKEN=machine-secret"}) {
		t.Fatalf("environment=%q err=%v", got, err)
	}
}

func TestProductionHelperRequiresHTTPSControl(t *testing.T) {
	base := map[string]string{"PAPERBOAT_RUNTIME_STATE_ROOT": filepath.Join(t.TempDir(), "state")}
	base["PAPERBOAT_WORKSPACE_ROOT"] = t.TempDir()
	base["PAPERBOAT_CONTROL_URL"] = "http://control.example.test"
	base["PAPERBOAT_MACHINE_ID"] = "um_1"
	if _, err := NewProductionHost(context.Background(), "test", func(name string) string { return base[name] }); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("machine control error=%v", err)
	}
}

func TestValidatedMachineShellRequiresExecutableAbsoluteFile(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := validatedMachineShell(shell); err != nil || got != shell {
		t.Fatalf("shell=%q err=%v", got, err)
	}
	for _, invalid := range []string{"relative", filepath.Join(t.TempDir(), "missing")} {
		if _, err := validatedMachineShell(invalid); !errors.Is(err, ErrProductionInvalid) {
			t.Fatalf("invalid shell %q err=%v", invalid, err)
		}
	}
	wantDefault, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := validatedMachineShell(""); err != nil || got != wantDefault {
		t.Fatalf("fallback shell=%q err=%v", got, err)
	}
}

func TestValidateMachineWorkspaceRejectsNonCanonicalAndSymlinkRoots(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMachineWorkspace(root); err != nil {
		t.Fatal(err)
	}
	if err := validateMachineWorkspace(root + string(os.PathSeparator) + "."); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("non-canonical error=%v", err)
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := validateMachineWorkspace(link); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("symlink error=%v", err)
	}
	if err := validateMachineWorkspace("relative"); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("relative error=%v", err)
	}
}

func TestWaitForPeerEnrollmentRetriesPendingUntilApproved(t *testing.T) {
	enrollment := &peerEnrollmentSequence{errors: []error{
		&peeridentityenrollment.PendingError{RequestID: "per_01", SafetyCode: "abcde-f0123"},
		peeridentityenrollment.ErrPending,
		nil,
	}}
	if err := waitForPeerEnrollment(context.Background(), enrollment, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if enrollment.calls != 3 {
		t.Fatalf("calls=%d", enrollment.calls)
	}
}

func TestWaitForPeerEnrollmentStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	enrollment := &peerEnrollmentSequence{errors: []error{peeridentityenrollment.ErrPending}}
	if err := waitForPeerEnrollment(ctx, enrollment, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if enrollment.calls != 1 {
		t.Fatalf("calls=%d", enrollment.calls)
	}
}

func TestPeerEnrollmentRuntimeServicePersistsApprovalAfterStartup(t *testing.T) {
	enrollment := &peerEnrollmentSequence{errors: []error{
		peeridentityenrollment.ErrPending,
		nil,
	}}
	service := newPeerEnrollmentRuntimeService(enrollment, time.Millisecond)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.done:
	case <-time.After(time.Second):
		t.Fatal("peer enrollment runtime service did not observe approval")
	}
	if enrollment.calls != 2 {
		t.Fatalf("calls=%d", enrollment.calls)
	}
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

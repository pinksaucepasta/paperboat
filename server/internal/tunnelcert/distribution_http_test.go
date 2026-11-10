package tunnelcert

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type distributionFaultStore struct{ records chan supportreport.Record }

type distributionBodyFailure struct {
	value    []byte
	readErr  error
	closeErr error
}

func (b *distributionBodyFailure) Read(p []byte) (int, error) {
	if len(b.value) == 0 {
		return 0, b.readErr
	}
	n := copy(p, b.value)
	b.value = b.value[n:]
	return n, b.readErr
}

func (b *distributionBodyFailure) Close() error { return b.closeErr }

func (s distributionFaultStore) Save(ctx context.Context, record supportreport.Record) error {
	select {
	case s.records <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (distributionFaultStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (distributionFaultStore) Cleanup(context.Context) error { return nil }

func TestDistributionHubWaitExpiresWithoutAnotherEdgeRequest(t *testing.T) {
	store := NewMemoryDistributionActionStore()
	_, _ = store.Put(context.Background(), DistributionAction{Key: distributionStoreKey("stale-process"), Message: DistributionMessage{ExpiresAt: time.Now().UTC().Add(25 * time.Millisecond)}})
	hub := &DistributionHub{now: func() time.Time { return time.Now().UTC() }, store: store}
	done := make(chan error, 1)
	go func() { done <- hub.waitStatus(context.Background(), "stale-process", "active") }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDistributionTransportStale) {
			t.Fatalf("waitStatus = %v, want stale", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waitStatus did not wake when the target process's message expired")
	}
}

func TestDistributionIdentityOutageIsCapturedAndPureRejectionRemainsUnauthorized(t *testing.T) {
	store := distributionFaultStore{records: make(chan supportreport.Record, 2)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	identityFailure := errors.Join(ErrDistributionIdentityInvalid, syscall.ECONNRESET)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte("distribution-credential-012345678901234567890123456789"),
		Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{}, identityFailure
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_distribution_identity")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_distribution_identity")
	ctx = reporter.Context(ctx, ref)
	request := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, strings.NewReader(`{}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer distribution-credential-012345678901234567890123456789")
	response := httptest.NewRecorder()
	hub.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "connection reset") {
		t.Fatalf("identity lookup outage response=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case record := <-store.records:
		if record.Reference != ref || record.RequestID != "req_distribution_identity" || record.CorrelationID != "corr_distribution_identity" || record.Code != "control_unavailable" || record.Cause != "connection_reset" || record.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("identity lookup report lost its cause or reference: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("identity lookup outage was not captured locally")
	}

	hub.identity = DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
		return DistributionNodeIdentity{}, ErrDistributionIdentityInvalid
	})
	denied := httptest.NewRecorder()
	denialRequest := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, strings.NewReader(`{}`)).WithContext(ctx)
	denialRequest.Header.Set("Authorization", "Bearer distribution-credential-012345678901234567890123456789")
	hub.Handler().ServeHTTP(denied, denialRequest)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("pure identity rejection status=%d, want %d", denied.Code, http.StatusUnauthorized)
	}
	select {
	case record := <-store.records:
		t.Fatalf("pure identity rejection generated exception report: %+v", record)
	default:
	}
}

func TestSignedDistributionBodyIOFailurePreservesReadAndCloseCauses(t *testing.T) {
	store := distributionFaultStore{records: make(chan supportreport.Record, 1)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewSignedDistributionNodeIdentityResolver(distributionTestKeyLookup{keys: map[string]ed25519.PublicKey{"edge_01\x00epoch_0001": publicKey}}, newTestNonces(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	const credential = "distribution-credential-012345678901234567890123456789"
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test", Credential: []byte(credential), Now: func() time.Time { return now }, Identity: resolver})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"edge_node_id":"edge_01","edge_process_epoch":"epoch_0001","limit":1}`)
	request := signedDistributionRequest(t, CertificateDistributionPullPath, "edge_01", "epoch_0001", now, distributionTestNonce(71), body, privateKey)
	request.Header.Set("Authorization", "Bearer "+credential)
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_distribution_body_io")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_distribution_body_io")
	ref := supportreport.NewReference()
	request = request.WithContext(reporter.Context(ctx, ref))
	request.Body = &distributionBodyFailure{value: body[:len(body)/2], readErr: syscall.EIO, closeErr: syscall.ECONNRESET}
	response := httptest.NewRecorder()
	hub.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "connection reset") {
		t.Fatalf("distribution body I/O response = %d %s", response.Code, response.Body.String())
	}
	select {
	case record := <-store.records:
		countErrnos := 0
		for _, item := range record.ErrorChain {
			if item == "Errno" {
				countErrnos++
			}
		}
		if record.Reference != ref || record.RequestID != "req_distribution_body_io" || record.CorrelationID != "corr_distribution_body_io" || record.Code != "control_unavailable" || record.HTTPStatus != http.StatusServiceUnavailable || countErrnos < 1 || record.Errno != int(syscall.ECONNRESET) {
			t.Fatalf("distribution body failure lost one of its causes or its reference: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("distribution body I/O failure was not captured locally")
	}
}

func TestDistributionHubBindsPullToAuthenticatedNode(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte("distribution-credential-012345678901234567890123456789"),
		Now:        func() time.Time { return now },
		Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, request *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: request.Header.Get("X-Paperboat-Edge-Node-ID"), ProcessEpoch: request.Header.Get("X-Paperboat-Edge-Process-Epoch")}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(24 * time.Hour),
	}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}}
	if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(hub.Handler())
	defer server.Close()

	pull := func(node, epoch string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(distributionPull{EdgeNodeID: node, EdgeProcessEpoch: epoch, Limit: 1})
		req := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer distribution-credential-012345678901234567890123456789")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Paperboat-Edge-Node-ID", node)
		req.Header.Set("X-Paperboat-Edge-Process-Epoch", epoch)
		recorder := httptest.NewRecorder()
		hub.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	crossNodeBody, _ := json.Marshal(distributionPull{EdgeNodeID: "edge_02", EdgeProcessEpoch: "epoch_0002", Limit: 1})
	crossNode := httptest.NewRequest(http.MethodPost, server.URL+CertificateDistributionPullPath, bytes.NewReader(crossNodeBody))
	crossNode.Header.Set("Authorization", "Bearer distribution-credential-012345678901234567890123456789")
	crossNode.Header.Set("Content-Type", "application/json")
	crossNode.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
	crossNode.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_0001")
	crossResponse := httptest.NewRecorder()
	hub.Handler().ServeHTTP(crossResponse, crossNode)
	if crossResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-node pull status = %d, want %d", crossResponse.Code, http.StatusForbidden)
	}

	response := pull("edge_01", "epoch_0001")
	if response.Code != http.StatusOK {
		t.Fatalf("bound pull status = %d", response.Code)
	}
	var document struct {
		Complete bool                  `json:"complete"`
		Messages []DistributionMessage `json:"messages"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if !document.Complete || len(document.Messages) != 1 {
		t.Fatalf("pull document = %+v", document)
	}
}

func TestDistributionHubRequestsExactLeafThroughAuthenticatedNode(t *testing.T) {
	credential := "distribution-credential-012345678901234567890123456789"
	var got DistributionNodeIdentity
	var gotHostname string
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte(credential),
		Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, request *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: request.Header.Get("X-Paperboat-Edge-Node-ID"), ProcessEpoch: request.Header.Get("X-Paperboat-Edge-Process-Epoch")}, nil
		}),
		OnDemand: func(_ context.Context, identity DistributionNodeIdentity, hostname string) error {
			got = identity
			gotHostname = hostname
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(distributionRequest{Hostname: "leaf.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, CertificateDistributionRequestPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
	request.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_0001")
	response := httptest.NewRecorder()
	hub.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("request status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if got != (DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}) || gotHostname != "leaf.example.test" {
		t.Fatalf("request callback = identity=%+v hostname=%q", got, gotHostname)
	}

	uppercaseBody, _ := json.Marshal(distributionRequest{Hostname: "Leaf.Example.Test"})
	uppercaseRequest := httptest.NewRequest(http.MethodPost, CertificateDistributionRequestPath, bytes.NewReader(uppercaseBody))
	uppercaseRequest.Header.Set("Authorization", "Bearer "+credential)
	uppercaseRequest.Header.Set("Content-Type", "application/json")
	uppercaseRequest.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
	uppercaseRequest.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_0001")
	uppercaseResponse := httptest.NewRecorder()
	hub.Handler().ServeHTTP(uppercaseResponse, uppercaseRequest)
	if uppercaseResponse.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical request status = %d, want %d", uppercaseResponse.Code, http.StatusBadRequest)
	}
}

func TestDistributionHubOnDemandMixedFailureIsReportedOnceAsUnavailable(t *testing.T) {
	store := distributionFaultStore{records: make(chan supportreport.Record, 2)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	const credential = "distribution-credential-012345678901234567890123456789"
	callbackErr := error(ErrGenerationConflict)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte(credential),
		Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, request *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: request.Header.Get("X-Paperboat-Edge-Node-ID"), ProcessEpoch: request.Header.Get("X-Paperboat-Edge-Process-Epoch")}, nil
		}),
		OnDemand: func(context.Context, DistributionNodeIdentity, string) error { return callbackErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := supportreport.NewReference()
	ctx := serverdiagnostics.WithRequestID(runCtx, "req_distribution_ondemand")
	ctx = serverdiagnostics.WithCorrelationID(ctx, "corr_distribution_ondemand")
	ctx = reporter.Context(ctx, ref)
	newRequest := func() *http.Request {
		body, marshalErr := json.Marshal(distributionRequest{Hostname: "leaf.example.test"})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		request := httptest.NewRequest(http.MethodPost, CertificateDistributionRequestPath, bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
		request.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_0001")
		return request
	}

	pure := httptest.NewRecorder()
	hub.Handler().ServeHTTP(pure, newRequest())
	if pure.Code != http.StatusConflict {
		t.Fatalf("pure generation conflict status=%d, want %d", pure.Code, http.StatusConflict)
	}
	select {
	case record := <-store.records:
		t.Fatalf("pure expected conflict emitted an exception: %+v", record)
	default:
	}

	callbackErr = errors.Join(ErrGenerationConflict, syscall.EIO)
	mixed := httptest.NewRecorder()
	hub.Handler().ServeHTTP(mixed, newRequest())
	if mixed.Code != http.StatusServiceUnavailable || strings.Contains(mixed.Body.String(), "input/output error") {
		t.Fatalf("mixed on-demand failure status=%d body=%s", mixed.Code, mixed.Body.String())
	}
	select {
	case record := <-store.records:
		if record.Reference != ref || record.RequestID != "req_distribution_ondemand" || record.CorrelationID != "corr_distribution_ondemand" || record.Code != "control_unavailable" || record.HTTPStatus != http.StatusServiceUnavailable || record.Errno != int(syscall.EIO) {
			t.Fatalf("mixed on-demand report lost its owner, cause, or reference: %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("mixed on-demand failure was not captured")
	}
	select {
	case record := <-store.records:
		t.Fatalf("one on-demand failure produced duplicate exception reports: %+v", record)
	default:
	}
}

func TestDistributionMessageTTLIsIndependentFromCertificateExpiry(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte("distribution-credential-012345678901234567890123456789"),
		Now:        func() time.Time { return now },
		Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, _ *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	certificateExpiry := now.Add(90 * 24 * time.Hour)
	request := DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: certificateExpiry,
	}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}}
	if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	message, err := hub.enqueue(context.Background(), "activate", DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: certificateExpiry,
	}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if got := message.ExpiresAt.Sub(now); got != maxDistributionLifetime {
		t.Fatalf("transport TTL = %s, want %s", got, maxDistributionLifetime)
	}
	if !message.CertificateExpiresAt.Equal(certificateExpiry) {
		t.Fatalf("certificate expiry = %s, want %s", message.CertificateExpiresAt, certificateExpiry)
	}

	now = now.Add(maxDistributionLifetime + time.Second)
	if err := hub.waitStatus(context.Background(), distributionKey("activate", DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1},
	}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}}), "active"); err == nil {
		t.Fatal("expired transport message remained waitable")
	}
}

func TestDistributionHubExpiresWipesAndReclaimsPendingCapacity(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential:     []byte("distribution-credential-012345678901234567890123456789"),
		MaximumPending: 1,
		Now:            func() time.Time { return now },
		Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}, Bundle: CertificateBundle{CertificatePEM: []byte("certificate-secret"), PrivateKeyPEM: []byte("private-key-secret")}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}}
	if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	stored, found, err := hub.store.Get(context.Background(), distributionStoreKey(distributionKey("stage", request)))
	if err != nil || !found {
		t.Fatalf("staged action not stored: found=%v err=%v", found, err)
	}
	if stored.Message.CertificatePEM != nil || stored.Message.PrivateKeyPEM != nil || bytes.Contains(stored.SealedPrivateKey, []byte("private-key-secret")) || len(stored.SealedPrivateKey) == 0 {
		t.Fatal("the stored action holds plaintext key material")
	}
	now = now.Add(maxDistributionLifetime + time.Second)
	replacement := request
	replacement.Certificate.ID = "certificate_02"
	replacement.Bundle.CertificatePEM = []byte("replacement-certificate")
	replacement.Bundle.PrivateKeyPEM = []byte("replacement-private-key")
	if _, err := hub.enqueue(context.Background(), "stage", replacement); err != nil {
		t.Fatalf("expired pending capacity was not reclaimed: %v", err)
	}
}

func allZeroBytes(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func TestDistributionHubPagesPendingMessagesWithStableCursor(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte("distribution-credential-012345678901234567890123456789"),
		Now:        func() time.Time { return now },
		Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maxDistributionMessages+1; index++ {
		request := DistributionRequest{Certificate: StoredCertificate{
			ID: fmt.Sprintf("certificate_%03d", index), AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
			DomainGeneration: 1, CertificateGeneration: uint64(index + 1), Fingerprint: [32]byte{byte(index + 1)}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: uint64(index + 1)}}
		if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
			t.Fatalf("enqueue %d: %v", index, err)
		}
	}
	pull := func(cursor string) struct {
		Complete   bool                  `json:"complete"`
		NextCursor string                `json:"next_cursor"`
		Messages   []DistributionMessage `json:"messages"`
	} {
		body, _ := json.Marshal(distributionPull{EdgeNodeID: "edge_01", EdgeProcessEpoch: "epoch_0001", Limit: maxDistributionMessages, Cursor: cursor})
		req := httptest.NewRequest(http.MethodPost, CertificateDistributionPullPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
		req.Header.Set("X-Paperboat-Edge-Process-Epoch", "epoch_0001")
		req = req.WithContext(withDistributionNodeIdentity(context.Background(), DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}))
		recorder := httptest.NewRecorder()
		hub.handlePull(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("pull status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var result struct {
			Complete   bool                  `json:"complete"`
			NextCursor string                `json:"next_cursor"`
			Messages   []DistributionMessage `json:"messages"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := pull("")
	if first.Complete || len(first.Messages) != maxDistributionMessages || first.NextCursor == "" {
		t.Fatalf("first page = complete=%v messages=%d cursor=%q", first.Complete, len(first.Messages), first.NextCursor)
	}
	second := pull(first.NextCursor)
	if !second.Complete || len(second.Messages) != 1 || second.NextCursor != "" {
		t.Fatalf("second page = complete=%v messages=%d cursor=%q", second.Complete, len(second.Messages), second.NextCursor)
	}
}

func TestDistributionHubRejectsActionsWithoutExactStageAndCloseWakesWaiters(t *testing.T) {
	now := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: []byte("distribution-credential-012345678901234567890123456789"),
		Now:        func() time.Time { return now },
		Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := DistributionRequest{Certificate: StoredCertificate{
		ID: "certificate_01", AccountID: "account_01", TunnelID: "tunnel_01", DomainID: "domain_01", Hostname: "preview.example.test",
		DomainGeneration: 1, CertificateGeneration: 1, Fingerprint: [32]byte{1}, Issuer: "test", NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
	}, Target: DistributionTarget{NodeID: "edge_01", ProcessEpoch: "epoch_0001", Generation: 1}}
	if _, err := hub.enqueue(context.Background(), "activate", request); !errors.Is(err, ErrDistributionTransportStale) {
		t.Fatalf("unstaged action error = %v", err)
	}
	// Removal must outlive the purged stage result, or a certificate revoked later stays on its edges.
	for _, action := range []string{"retire", "revoke"} {
		if _, err := hub.enqueue(context.Background(), action, request); err != nil {
			t.Fatalf("%s without a stage record = %v, want queued", action, err)
		}
	}
	if _, err := hub.enqueue(context.Background(), "stage", request); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- hub.wait(context.Background(), distributionKey("stage", request)) }()
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitErr:
		if !errors.Is(err, ErrDistributionTransportStale) {
			t.Fatalf("closed wait error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake distribution waiter")
	}
	if _, err := hub.enqueue(context.Background(), "stage", request); !errors.Is(err, ErrDistributionTransportInvalid) {
		t.Fatalf("closed enqueue error = %v", err)
	}
}

func TestDistributionHubRequiresAuthenticatedNodeResolver(t *testing.T) {
	if _, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test", Credential: []byte("distribution-credential-012345678901234567890123456789")}); err == nil {
		t.Fatal("hub accepted body-only node identity")
	}
}

func TestDistributionHubCloseWipesOwnedCredential(t *testing.T) {
	credential := []byte("distribution-credential-012345678901234567890123456789")
	hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
		Credential: credential,
		Identity: DistributionNodeIdentityResolverFunc(func(context.Context, *http.Request) (DistributionNodeIdentity, error) {
			return DistributionNodeIdentity{NodeID: "edge_01", ProcessEpoch: "epoch_0001"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	owned := hub.credential
	if len(owned) == 0 || &owned[0] == &credential[0] {
		t.Fatal("hub did not take an owned credential copy")
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	if len(owned) == 0 || !allZeroBytes(owned) || hub.credential != nil {
		t.Fatalf("hub credential was not wiped: %q", owned)
	}
	if !bytes.Equal(credential, []byte("distribution-credential-012345678901234567890123456789")) {
		t.Fatal("closing hub modified caller credential")
	}
}

// cancelAwareCredentials answers node credential lookups the way a database query does: with the request's own
// context error once it is cancelled, otherwise with a fixed outcome.
type cancelAwareCredentials struct{ outage error }

func (c cancelAwareCredentials) RuntimeNode(ctx context.Context, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", c.outage
}
func (c cancelAwareCredentials) RuntimeCredential(context.Context, string) (string, error) {
	return "", c.outage
}
func (cancelAwareCredentials) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }

// An edge cancels its on-demand certificate request when the client TLS handshake behind it ends. The hub must
// answer 499 and report nothing (production logged these as 503 control_unavailable after every new preview
// certificate, on .6, .7 and .8); a real credential outage on a live request is still 503 and reported.
func TestDistributionRequestCancelledByTheEdgeIsNotAControlPlaneFailure(t *testing.T) {
	store := distributionFaultStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, stopReporter := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { stopReporter(); <-done }()

	const credential = "distribution-credential-012345678901234567890123456789"
	identity, err := NewSignedDistributionNodeIdentityResolver(distributionTestKeyLookup{keys: map[string]ed25519.PublicKey{}}, newTestNonces(), nil)
	if err != nil {
		t.Fatal(err)
	}
	send := func(outage error, cancelled bool) int {
		hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
			Credential: []byte(credential), NodeCredentials: cancelAwareCredentials{outage: outage}, Identity: identity})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, CertificateDistributionRequestPath, strings.NewReader(`{"hostname":"fresh-preview.example.test"}`))
		request.Header.Set("Authorization", "Bearer a-self-hosted-edge-runtime-credential-0123456789")
		request.Header.Set("X-Paperboat-Edge-Node-ID", "edge_01")
		ctx, cancel := context.WithCancel(reporter.Context(runCtx, supportreport.NewReference()))
		if cancelled {
			cancel()
		} else {
			defer cancel()
		}
		response := httptest.NewRecorder()
		hub.Handler().ServeHTTP(response, request.WithContext(ctx))
		return response.Code
	}
	if code := send(errors.New("database unavailable"), true); code != statusClientClosedRequest {
		t.Fatalf("a request the edge cancelled answered %d, want 499", code)
	}
	select {
	case record := <-store.records:
		t.Fatalf("a request the edge cancelled was reported as a failure: %+v", record)
	case <-time.After(300 * time.Millisecond):
	}
	if code := send(errors.New("database unavailable"), false); code != http.StatusServiceUnavailable {
		t.Fatalf("a credential outage on a live request answered %d, want 503", code)
	}
	select {
	case record := <-store.records:
		if record.Code != "control_unavailable" {
			t.Fatalf("outage reported as %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("a real credential outage was not reported")
	}
}

// Both edges request a new preview's exact leaf during its first handshakes. Production 2026.11.05.8 answered every
// request that arrived during the issuance 503 (17 in two bursts), so those handshakes failed. Now a request that
// finds the leaf already being issued waits for that issuance (certmagic's on-demand wait) and gets the certificate:
// two concurrent requests, one issuance, both succeed, nothing reported. Only a wait that runs out is a 409 (typed,
// not reported); a real issuer outage stays a reported 503.
func TestDistributionOnDemandWaitsForTheLeafAlreadyBeingIssued(t *testing.T) {
	store := distributionFaultStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { reporter.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	const credential = "distribution-credential-012345678901234567890123456789"
	newHub := func(onDemand func(context.Context, DistributionNodeIdentity, string) error) *DistributionHub {
		hub, err := NewDistributionHub(DistributionHubConfig{Store: NewMemoryDistributionActionStore(), Keys: testSecretSource{value: bytes.Repeat([]byte{7}, 32)}, KeyReference: "distribution-test",
			Credential: []byte(credential),
			Identity: DistributionNodeIdentityResolverFunc(func(_ context.Context, request *http.Request) (DistributionNodeIdentity, error) {
				return DistributionNodeIdentity{NodeID: request.Header.Get("X-Paperboat-Edge-Node-ID"), ProcessEpoch: "epoch_0001"}, nil
			}),
			OnDemand: onDemand,
		})
		if err != nil {
			t.Fatal(err)
		}
		return hub
	}
	send := func(hub *DistributionHub, edge string, ctx context.Context) int {
		body, _ := json.Marshal(distributionRequest{Hostname: "fresh-preview.example.test"})
		request := httptest.NewRequest(http.MethodPost, CertificateDistributionRequestPath, bytes.NewReader(body)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Paperboat-Edge-Node-ID", edge)
		response := httptest.NewRecorder()
		hub.Handler().ServeHTTP(response, request)
		return response.Code
	}
	// One issuance at a time, like the coordinator's shared issuance lock: the first caller issues for 600 ms.
	var mu sync.Mutex
	issuing, issued, issuances := false, false, 0
	hub := newHub(func(context.Context, DistributionNodeIdentity, string) error {
		mu.Lock()
		if issued {
			mu.Unlock()
			return nil
		}
		if issuing {
			mu.Unlock()
			return ErrIssuanceLocked
		}
		issuing, issuances = true, issuances+1
		mu.Unlock()
		time.Sleep(600 * time.Millisecond)
		mu.Lock()
		issuing, issued = false, true
		mu.Unlock()
		return nil
	})
	codes := make(chan int, 2)
	for _, edge := range []string{"edge_01", "edge_02"} {
		go func() { codes <- send(hub, edge, reporter.Context(runCtx, supportreport.NewReference())) }()
	}
	for range 2 {
		if code := <-codes; code != http.StatusNoContent {
			t.Fatalf("a concurrent first request answered %d, want 204 after the in-flight issuance", code)
		}
	}
	if issuances != 1 {
		t.Fatalf("issuances = %d, want 1", issuances)
	}
	stalled := newHub(func(context.Context, DistributionNodeIdentity, string) error { return ErrIssuanceLocked })
	short, stop := context.WithTimeout(reporter.Context(runCtx, supportreport.NewReference()), time.Second)
	defer stop()
	if code := send(stalled, "edge_01", short); code != http.StatusConflict {
		t.Fatalf("a wait that ran out answered %d, want 409", code)
	}
	select {
	case record := <-store.records:
		t.Fatalf("waiting for an issuance was reported as a failure: %+v", record)
	case <-time.After(300 * time.Millisecond):
	}
	// Cancellation that lands inside the callback: the edge leaving is a 499, our own wait running out a 409; neither is reported.
	blocked := newHub(func(ctx context.Context, _ DistributionNodeIdentity, _ string) error { <-ctx.Done(); return ctx.Err() })
	gone, leave := context.WithCancel(reporter.Context(runCtx, supportreport.NewReference()))
	time.AfterFunc(100*time.Millisecond, leave)
	if code := send(blocked, "edge_01", gone); code != statusClientClosedRequest {
		t.Fatalf("an edge that left during issuance answered %d, want 499", code)
	}
	wait := onDemandWait
	onDemandWait = 200 * time.Millisecond
	code := send(blocked, "edge_01", reporter.Context(runCtx, supportreport.NewReference()))
	onDemandWait = wait
	if code != http.StatusConflict {
		t.Fatalf("a wait that ran out inside issuance answered %d, want 409", code)
	}
	select {
	case record := <-store.records:
		t.Fatalf("a cancelled issuance was reported as a failure: %+v", record)
	case <-time.After(300 * time.Millisecond):
	}
	broken := newHub(func(context.Context, DistributionNodeIdentity, string) error { return ErrIssuerUnavailable })
	if code := send(broken, "edge_01", reporter.Context(runCtx, supportreport.NewReference())); code != http.StatusServiceUnavailable {
		t.Fatalf("issuer unavailable answered %d, want 503", code)
	}
	select {
	case record := <-store.records:
		if record.Code != "control_unavailable" {
			t.Fatalf("issuer outage reported as %+v", record)
		}
	case <-time.After(time.Second):
		t.Fatal("a real issuance failure was not reported")
	}
}

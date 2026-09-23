//go:build darwin || linux

package runtime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

type projectionTestCredentials struct {
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

func (c *projectionTestCredentials) Token(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

func (c *projectionTestCredentials) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
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

func (c *projectionTestCredentials) snapshot() (operation, method, path string, body []byte, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operation, c.method, c.path, bytes.Clone(c.body), c.calls
}

type projectionRegistrationRequest struct {
	OperationID            string `json:"operation_id"`
	InstallationGeneration uint64 `json:"installation_generation"`
	HostKeyGeneration      uint64 `json:"host_key_generation"`
	HostPublic             string `json:"host_public"`
}

func signedProjectionResponse(issuer string, request projectionRegistrationRequest, machineID string, values map[string][]byte) ([]byte, envinject.ProjectionBundle, error) {
	hostPublic, err := base64.RawURLEncoding.Strict().DecodeString(request.HostPublic)
	if err != nil || len(hostPublic) != 32 {
		return nil, envinject.ProjectionBundle{}, errors.New("invalid host public in registration request")
	}
	writerSeed := bytes.Repeat([]byte{0x49}, ed25519.SeedSize)
	defer clear(writerSeed)
	projection, err := environmente2ee.SealHostProjection(context.Background(), environmente2ee.HostProjectionClaims{
		Issuer:                 issuer,
		OwnerAccount:           "account_1",
		MachineID:              machineID,
		InstallationGeneration: request.InstallationGeneration,
		HostKeyGeneration:      request.HostKeyGeneration,
		HostPublic:             hostPublic,
		SelectionGeneration:    1,
		Revision:               1,
		Previous:               make([]byte, 32),
		Sources:                []environmente2ee.ProjectionSource{},
		WriterVaultGeneration:  1,
	}, writerSeed, values)
	if err != nil {
		return nil, envinject.ProjectionBundle{}, err
	}
	bundle := envinject.ProjectionBundle{
		Schema:                 envinject.ProjectionBundleSchema,
		AccountID:              "account_1",
		MachineID:              machineID,
		InstallationGeneration: request.InstallationGeneration,
		HostKeyGeneration:      request.HostKeyGeneration,
		HostPublic:             request.HostPublic,
		WriterPublic:           base64.RawURLEncoding.EncodeToString(projection.Claims.WriterPublic),
		FenceGeneration:        1,
		SelectionGeneration:    1,
		ProjectionRevision:     1,
		DocumentID:             projection.ID.String(),
		Envelope:               base64.RawURLEncoding.EncodeToString(projection.Raw),
		State:                  "ready",
	}
	body, err := json.Marshal(struct {
		Data envinject.ProjectionBundle `json:"data"`
	}{Data: bundle})
	return body, bundle, err
}

func projectionServiceURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func projectionRegistration(t *testing.T) runtimeidentity.Registration {
	t.Helper()
	return runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 11}
}

func waitProjectionEnvironment(t *testing.T, service *projectionEnvironmentService, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := service.Environment()
		if err == nil {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("environment=%q want=%q", got, want)
			}
			return
		}
		if !errors.Is(err, envinject.ErrNotReady) {
			t.Fatalf("environment error=%v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("environment did not become ready: want=%q", want)
}

func projectionShutdown(t *testing.T, service *projectionEnvironmentService) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := service.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionEnvironmentServiceRegistersExactMachineControlAndClosesStoreWithCanceledContext(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	stateRoot := filepath.Join(t.TempDir(), "runtime")
	registration := projectionRegistration(t)
	credentials := &projectionTestCredentials{token: "test-token", proof: []byte("machine-proof"), started: make(chan struct{})}
	requestErrors := make(chan error, 1)
	bundles := make(chan envinject.ProjectionBundle, 1)
	requestBodies := make(chan []byte, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/environment/hosts/machine_1/key" {
			requestErrors <- errors.New("machine-control method or path changed")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			requestErrors <- errors.New("machine-control authorization changed")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := request.Header.Get("Cache-Control"); got != "no-store" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			requestErrors <- errors.New("machine-control cache or content headers changed")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			requestErrors <- err
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var registrationRequest projectionRegistrationRequest
		if err := json.Unmarshal(raw, &registrationRequest); err != nil || registrationRequest.OperationID == "" || registrationRequest.HostPublic == "" {
			requestErrors <- errors.New("invalid machine-control body")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := request.Header.Get("Idempotency-Key"); got != registrationRequest.OperationID {
			requestErrors <- errors.New("idempotency key does not match operation body")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := request.Header.Get("X-Paperboat-Machine-Proof"); got != base64.RawURLEncoding.EncodeToString(credentials.proof) {
			requestErrors <- errors.New("machine proof header changed")
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		requestBodies <- bytes.Clone(raw)
		body, bundle, err := signedProjectionResponse(server.URL, registrationRequest, registration.MachineID, map[string][]byte{"BOOT": []byte("ready")})
		if err != nil {
			requestErrors <- err
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		bundles <- bundle
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(body)
	}))
	defer server.Close()

	service := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-credentials.started:
	case <-time.After(3 * time.Second):
		t.Fatal("machine-control registration did not start")
	}
	waitProjectionEnvironment(t, service, []string{"BOOT=ready"})
	select {
	case err := <-requestErrors:
		t.Fatal(err)
	default:
	}

	operation, method, path, signedBody, calls := credentials.snapshot()
	if calls != 1 || operation == "" || method != http.MethodPost || path != "/v1/environment/hosts/machine_1/key" {
		t.Fatalf("proof input operation=%q method=%q path=%q calls=%d", operation, method, path, calls)
	}
	wireBody := <-requestBodies
	bundle := <-bundles
	if len(signedBody) == 0 {
		t.Fatal("registration body was not captured")
	}
	if !bytes.Equal(signedBody, wireBody) {
		t.Fatal("proof did not cover the exact wire body")
	}

	store := service.current()
	if store == nil {
		t.Fatal("bound projection store missing")
	}
	cacheBefore, err := os.ReadFile(filepath.Join(stateRoot, "environment", "projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	floorBefore, err := os.ReadFile(filepath.Join(stateRoot, "environment-projection-high-water.json"))
	if err != nil {
		t.Fatal(err)
	}
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	cancelShutdown()
	if err := service.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled shutdown error=%v", err)
	}
	if _, err := service.Environment(); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed service environment error=%v", err)
	}
	if service.BindingState() != envinject.BindingUnknown {
		t.Fatalf("closed service binding state=%d", service.BindingState())
	}
	if _, err := service.NextObservation(time.Now().UTC()); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed service observation error=%v", err)
	}
	if err := service.Apply(context.Background(), bundle); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed service apply error=%v", err)
	}
	if err := service.ensure(context.Background()); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("late registration error=%v", err)
	}
	if _, _, _, _, callsAfter := credentials.snapshot(); callsAfter != calls {
		t.Fatalf("late registration made a credential call: before=%d after=%d", calls, callsAfter)
	}
	if err := store.Apply(context.Background(), bundle); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed store apply error=%v", err)
	}
	if err := store.Bind(context.Background(), bundle); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed store bind error=%v", err)
	}
	if _, err := store.NextObservation(time.Now().UTC()); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed store observation error=%v", err)
	}
	cacheAfter, err := os.ReadFile(filepath.Join(stateRoot, "environment", "projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	floorAfter, err := os.ReadFile(filepath.Join(stateRoot, "environment-projection-high-water.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cacheBefore, cacheAfter) || !bytes.Equal(floorBefore, floorAfter) {
		t.Fatal("closed projection operation changed durable state")
	}
}

func TestProjectionEnvironmentServiceRejectsInvalidAndForeignBindings(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	for _, test := range []struct {
		name    string
		invalid bool
		want    error
	}{
		{name: "invalid", invalid: true, want: envinject.ErrInvalidSnapshot},
		{name: "foreign_machine", want: envinject.ErrInvalidSnapshot},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateRoot := filepath.Join(t.TempDir(), "runtime")
			registration := projectionRegistration(t)
			credentials := &projectionTestCredentials{token: "test-token", proof: []byte("proof")}
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
				if err != nil {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				if test.invalid {
					_, _ = response.Write([]byte(`{"data":{"schema":"invalid"}}`))
					return
				}
				var registrationRequest projectionRegistrationRequest
				if json.Unmarshal(raw, &registrationRequest) != nil {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				body, _, err := signedProjectionResponse(server.URL, registrationRequest, "foreign_machine", map[string][]byte{"BOOT": []byte("never")})
				if err != nil {
					response.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = response.Write(body)
			}))
			service := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
			defer server.Close()
			if err := service.restore(context.Background()); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if err := service.ensure(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("ensure error=%v want=%v", err, test.want)
			}
			if _, err := service.Environment(); !errors.Is(err, envinject.ErrNotReady) {
				t.Fatalf("invalid binding exposed environment: %v", err)
			}
			if service.current() != nil {
				t.Fatal("invalid binding installed a projection store")
			}
			server.Close()
		})
	}
}

func TestProjectionEnvironmentServiceRestoresCachedEnvironmentBeforeOfflineRetry(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	stateRoot := filepath.Join(t.TempDir(), "runtime")
	registration := projectionRegistration(t)
	credentials := &projectionTestCredentials{token: "test-token", proof: []byte("proof")}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var registrationRequest projectionRegistrationRequest
		if json.Unmarshal(raw, &registrationRequest) != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		body, _, err := signedProjectionResponse(server.URL, registrationRequest, registration.MachineID, map[string][]byte{"OFFLINE": []byte("cached")})
		if err != nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = response.Write(body)
	}))
	defer server.Close()
	seed := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	if err := seed.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := seed.Environment(); err != nil || !reflect.DeepEqual(got, []string{"OFFLINE=cached"}) {
		t.Fatalf("seed environment=%q err=%v", got, err)
	}
	seedStore := seed.current()
	if seedStore == nil {
		t.Fatal("seed store missing")
	}

	offlineCredentials := &projectionTestCredentials{token: "offline-token", proof: []byte("offline-proof")}
	offlineTransport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("control plane offline")
	})
	// The issuer is part of the authenticated projection claims, so an offline
	// restart keeps the same control URL while its transport refuses requests.
	offline := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), offlineTransport, registration, offlineCredentials)
	if err := offline.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitProjectionEnvironment(t, offline, []string{"OFFLINE=cached"})
	projectionShutdown(t, offline)
	seedStore.Close()
}

func TestProjectionEnvironmentServiceShutdownCancelsRegistrationAndClearsRestoredValues(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	stateRoot := filepath.Join(t.TempDir(), "runtime")
	registration := projectionRegistration(t)
	seedCredentials := &projectionTestCredentials{token: "seed-token", proof: []byte("seed-proof")}
	seedBundles := make(chan envinject.ProjectionBundle, 1)
	registrationStarted := make(chan struct{})
	registrationDone := make(chan struct{})
	releaseRegistration := make(chan struct{})
	var registrationOnce sync.Once
	var registrationDoneOnce sync.Once
	var requestCountMu sync.Mutex
	requestCount := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		requestCountMu.Lock()
		requestCount++
		count := requestCount
		requestCountMu.Unlock()
		if count > 1 {
			registrationOnce.Do(func() { close(registrationStarted) })
			select {
			case <-request.Context().Done():
			case <-releaseRegistration:
			}
			registrationDoneOnce.Do(func() { close(registrationDone) })
			return
		}
		var registrationRequest projectionRegistrationRequest
		if json.Unmarshal(raw, &registrationRequest) != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		body, bundle, err := signedProjectionResponse(server.URL, registrationRequest, registration.MachineID, map[string][]byte{"SECRET": []byte("cached")})
		if err != nil {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		seedBundles <- bundle
		_, _ = response.Write(body)
	}))
	defer close(releaseRegistration)
	defer server.Close()
	seed := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), server.Client().Transport, registration, seedCredentials)
	if err := seed.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle := <-seedBundles
	seedStore := seed.current()
	if seedStore == nil {
		t.Fatal("seed store missing")
	}
	seedStore.Close()
	credentials := &projectionTestCredentials{token: "test-token", proof: []byte("proof")}
	service := newProjectionEnvironmentService(stateRoot, projectionServiceURL(t, server.URL), server.Client().Transport, registration, credentials)
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitProjectionEnvironment(t, service, []string{"SECRET=cached"})
	store := service.current()
	if store == nil {
		t.Fatal("restored projection store missing")
	}
	cacheBefore, err := os.ReadFile(filepath.Join(stateRoot, "environment", "projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	floorBefore, err := os.ReadFile(filepath.Join(stateRoot, "environment-projection-high-water.json"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-registrationStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("blocking registration did not start")
	}
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	cancelShutdown()
	if err := service.Shutdown(shutdownCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled shutdown error=%v", err)
	}
	select {
	case <-registrationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel the in-flight registration")
	}
	if _, err := service.Environment(); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("shutdown environment error=%v", err)
	}
	if service.BindingState() != envinject.BindingUnknown {
		t.Fatalf("shutdown binding state=%d", service.BindingState())
	}
	if err := store.Apply(context.Background(), bundle); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed restored store apply error=%v", err)
	}
	if err := store.Bind(context.Background(), bundle); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed restored store bind error=%v", err)
	}
	if _, err := store.NextObservation(time.Now().UTC()); !errors.Is(err, envinject.ErrNotReady) {
		t.Fatalf("closed restored store observation error=%v", err)
	}
	cacheAfter, err := os.ReadFile(filepath.Join(stateRoot, "environment", "projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	floorAfter, err := os.ReadFile(filepath.Join(stateRoot, "environment-projection-high-water.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cacheBefore, cacheAfter) || !bytes.Equal(floorBefore, floorAfter) {
		t.Fatal("canceled shutdown allowed a closed store to rewrite durable state")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// A supervisor cancels startup once the service is accepted. Registration must
// remain live until Shutdown, even if its first response is still in flight.
func TestProjectionEnvironmentServiceOutlivesStartupContext(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	registration := projectionRegistration(t)
	credentials := &projectionTestCredentials{token: "test-token", proof: []byte("proof")}
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var remote *httptest.Server
	remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request projectionRegistrationRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			w.WriteHeader(400)
			return
		}
		once.Do(func() { close(arrived) })
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		raw, _, err := signedProjectionResponse(remote.URL, request, registration.MachineID, map[string][]byte{"BOOT": []byte("ready")})
		if err != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write(raw)
	}))
	defer remote.Close()
	service := newProjectionEnvironmentService(filepath.Join(t.TempDir(), "runtime"), projectionServiceURL(t, remote.URL), remote.Client().Transport, registration, credentials)
	startup, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(startup); err != nil {
		t.Fatal(err)
	}
	defer projectionShutdown(t, service)
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("registration did not start")
	}
	cancel()
	close(release)
	waitProjectionEnvironment(t, service, []string{"BOOT=ready"})
}

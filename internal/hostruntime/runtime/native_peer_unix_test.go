//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestProductionNativePeerSupervisorRestartsAndShutdownRemainsAwaitable(t *testing.T) {
	first := &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1)}
	second := &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1)}
	release := make(chan struct{})
	second.done.Add(1)
	go func() {
		<-release
		second.done.Done()
	}()
	var starts atomic.Int32
	service := &productionNativePeerService{}
	service.startGeneration = func(context.Context) (*productionNativePeerGeneration, error) {
		if starts.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	first.errors <- errors.New("listener failed")
	deadline := time.Now().Add(3 * time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if starts.Load() != 2 {
		t.Fatal("failed generation was not replaced")
	}
	timeout, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := service.Shutdown(timeout); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown while cleanup blocked = %v", err)
	}
	close(release)
	complete, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := service.Shutdown(complete); err != nil {
		t.Fatalf("second shutdown did not await existing cleanup: %v", err)
	}
}

func TestProductionNativePeerDispatchesPrivateTCPThroughCurrentState(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	binding := nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tunnel_1", ResourceGeneration: 1, RouteID: "route_1", RouteGeneration: 2, TargetGeneration: 3, OwnerEndpointID: "machine_1", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:4321", ExpiresAt: expires}
	target, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.NewNativePrivate("operation_1", "private_tcp", "stream_1", "credential", expires, 1024, target)
	if err != nil {
		t.Fatal(err)
	}
	client, host := net.Pipe()
	origin, backend := net.Pipe()
	defer client.Close()
	defer backend.Close()
	service := &productionNativePeerService{config: productionNativePeerConfig{
		privateCurrent: func(context.Context, nativeprivate.Binding) (time.Time, <-chan struct{}, error) {
			return expires, make(chan struct{}), nil
		},
		privateDial: func(context.Context, string, string) (net.Conn, error) { return origin, nil },
	}}
	done := make(chan error, 1)
	go func() { done <- service.serveStream(t.Context(), header, host) }()
	var ready [1]byte
	if _, err := io.ReadFull(client, ready[:]); err != nil || ready[0] != 0 {
		t.Fatalf("private TCP readiness = %v, %v", ready, err)
	}
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	request := make([]byte, len("request"))
	if _, err := io.ReadFull(backend, request); err != nil || string(request) != "request" {
		t.Fatalf("origin request = %q, %v", request, err)
	}
	_ = client.Close()
	_ = backend.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("private TCP dispatch did not stop")
	}
}

func TestProductionNativePeerStartFailureCanRetry(t *testing.T) {
	service := &productionNativePeerService{}
	var starts atomic.Int32
	service.startGeneration = func(context.Context) (*productionNativePeerGeneration, error) {
		if starts.Add(1) == 1 {
			return nil, errors.New("identity pending")
		}
		return &productionNativePeerGeneration{cancel: func() {}, errors: make(chan error, 1)}, nil
	}
	if err := service.Start(t.Context()); err == nil {
		t.Fatal("initial generation failure was hidden")
	}
	if err := service.Start(t.Context()); err != nil {
		t.Fatalf("failed start left service permanently busy: %v", err)
	}
	if err := service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestProductionNativePeerReloadsRenewedEndpointIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	key := store.Current()
	if err := store.SaveRegistration(runtimeidentity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_1", EnvironmentID: "environment_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, SetupRoles: []string{"host"}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.PeerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	rootPublic, rootPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial uint64) string {
		certificate, signErr := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: "account_1", Role: endpointidentity.RoleMachine, EndpointID: "machine_1", NoisePublicKey: endpoint.NoisePublicKey(), QUICPublicKey: endpoint.QUICPublicKey(), Generation: 1, Serial: serial, IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
		if signErr != nil {
			t.Fatal(signErr)
		}
		raw, marshalErr := certificate.MarshalBinary()
		if marshalErr != nil {
			t.Fatal("save endpoint certificate", marshalErr)
		}
		if saveErr := store.SavePeerEndpointCertificate(rootPublic, raw, now); saveErr != nil {
			t.Fatal("save endpoint certificate", saveErr)
		}
		return certificate.Fingerprint()
	}
	first := issue(1)
	service := &productionNativePeerService{config: productionNativePeerConfig{stateRoot: root, machineID: "machine_1", generation: 1}}
	if got, err := service.currentIdentityFingerprint(); err != nil || got != first {
		t.Fatalf("initial fingerprint = %q, %v", got, err)
	}
	second := issue(2)
	if got, err := service.currentIdentityFingerprint(); err != nil || got != second || got == first {
		t.Fatalf("renewed fingerprint = %q, %v", got, err)
	}
}

func TestNativeNetworkAuthorizerUsesVerifiedAccessSessionInsteadOfJournalHash(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	factory, err := NewStaticAuthorizer(StaticAuthConfig{Issuer: "https://control.test", EnvironmentID: "env_test", MachineID: "machine_test", HelperID: "hlp_test", Keys: map[string]ed25519.PublicKey{"key-1": public}, Clock: staticClock{now}})
	if err != nil {
		t.Fatal(err)
	}
	authorize := nativeNetworkAuthorizer(factory)
	for consumer, policy := range map[string][2]string{
		"terminal":      {"terminal_operation", "terminal:operate"},
		"exec":          {"exec_operation", "exec:operate"},
		"ssh":           {"ssh_operation", "ssh:operate"},
		"file_transfer": {"file_transfer", "file:transfer"},
		"codex":         {"codex_connect", "codex:connect"},
	} {
		t.Run(consumer, func(t *testing.T) {
			class, scope := policy[0], policy[1]
			claims := auth.Claims{Issuer: "https://control.test", Audience: "paperboat-machine", Subject: "user_test", JTI: "jti_test", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Scope: []string{scope}, CredentialClass: class, EnvironmentID: "env_test", MachineID: "machine_test", SourceMachineID: "source_test", UserID: "user_test", CLIClientSessionID: "cli_test", SessionID: "terminal_session_test", AssignmentID: "access_test", OperationID: "operation_test"}
			if consumer == "codex" {
				claims.AssignmentID = ""
				claims.SessionID = "access_test"
			}
			header, err := streamauth.New("operation_test", consumer, "stream_test", signStaticCredential(t, private, "key-1", claims), now.Add(time.Minute), 1024)
			if err != nil {
				t.Fatal(err)
			}
			resource, err := authorize(context.Background(), header)
			if err != nil || resource != "access_test" {
				t.Fatalf("verified scope resource=%q want access_test err=%v", resource, err)
			}
			missing := claims
			missing.AssignmentID = ""
			if consumer == "codex" {
				missing.SessionID = ""
			}
			header.Credential = signStaticCredential(t, private, "key-1", missing)
			if resource, err = authorize(context.Background(), header); err == nil || resource != "" {
				t.Fatalf("missing network resource accepted resource=%q err=%v", resource, err)
			}
			claims.MachineID = "other_machine"
			header.Credential = signStaticCredential(t, private, "key-1", claims)
			if resource, err = authorize(context.Background(), header); err == nil || resource != "" {
				t.Fatalf("cross-machine credential accepted resource=%q err=%v", resource, err)
			}
		})
	}
}

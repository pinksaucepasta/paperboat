package preview

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
)

type sharedPreviewKey struct {
	key ed25519.PublicKey
	kid string
}

func (k sharedPreviewKey) Lookup(_ context.Context, id string) (ed25519.PublicKey, bool, error) {
	return k.key, id == k.kid, nil
}
func (k sharedPreviewKey) Refresh(context.Context) error { return nil }

type sharedPreviewClock time.Time

func (c sharedPreviewClock) Now() time.Time { return time.Time(c) }

// Consume server-minted shared authority using the real credential verifier,
// runtime owner registry, DispatchManager and connected data-carrier protocol.
// Database renewal/revocation/stop is checked by the producer; this consumer
// checks signed launch, carrier readiness and endpoint cleanup.
func TestServerIssuedSharedPreviewRuntime(t *testing.T) {
	path := os.Getenv("PAPERBOAT_SHARED_PREVIEW_FIXTURE")
	if path == "" {
		t.Skip("requires shared PostgreSQL producer fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	var f struct {
		Issuer, MachineIssuer, PublicKey, Token string
		Dispatch                                DispatchRequest
		Renewed                                 struct {
			Preview Lease
			ETag    string
		}
		Stopped struct {
			Preview Lease
			ETag    string
		}
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	clear(raw)
	block, _ := pem.Decode([]byte(f.PublicKey))
	if block == nil {
		t.Fatal("missing signing key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		t.Fatal("invalid signing key")
	}
	parts := strings.Split(f.Token, ".")
	if len(parts) != 3 {
		t.Fatal("invalid fixture credential")
	}
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h struct{ Kid string }
	json.Unmarshal(header, &h)
	body, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var timing struct {
		IssuedAt int64 `json:"iat"`
	}
	json.Unmarshal(body, &timing)
	clear(body)
	now := time.Unix(timing.IssuedAt, 0).Add(time.Second)
	verifier := auth.Verifier{Keys: sharedPreviewKey{key, h.Kid}, Clock: sharedPreviewClock(now)}
	claims, err := verifier.Verify(context.Background(), f.Token, auth.Policy{Issuer: f.Issuer, Audience: "paperboat-machine", CredentialClass: "preview_launch", MachineID: f.Dispatch.OwnerDeviceID, OperationID: f.Dispatch.OperationID, Scopes: []string{"preview:launch"}})
	if err != nil {
		t.Fatal(err)
	}
	if claims.AccountID == f.MachineIssuer || claims.AccountID != f.Dispatch.AccountID || claims.UserID != claims.ActorID {
		t.Fatal("shared issuer was substituted for requester")
	}
	authorization := DispatchAuthorization{AccountID: claims.AccountID, ActorID: claims.ActorID, MachineID: claims.MachineID, OwnerSessionID: claims.OwnerSessionID, PreviewID: claims.PreviewID, OperationID: claims.OperationID, ExpectedGeneration: claims.ExpectedGeneration, IdempotencyKey: claims.IdempotencyKey, RequestID: claims.RequestID, CorrelationID: claims.CorrelationID, RequestHash: claims.RequestHash, ExpiresAt: time.Unix(claims.ExpiresAt, 0)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", f.Dispatch.Target.Address)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("shared-preview-origin")) }))
	origin.Listener.Close()
	origin.Listener = listener
	origin.Start()
	defer origin.Close()
	identity := testPreviewCarrierIdentity(1)
	identity.AccountID = f.Dispatch.AccountID
	identity.HostID = f.Dispatch.OwnerDeviceID
	pair := newPreviewCarrierPair(t, ctx, identity)
	defer pair.close()
	carrier, err := NewDataCarrierPreviewCarrier(DataCarrierPreviewCarrierConfig{Active: pair.active, Identity: identity, RouteID: f.Dispatch.PreviewID})
	if err != nil {
		t.Fatal(err)
	}
	owners, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{AccountID: f.MachineIssuer, MachineID: f.Dispatch.OwnerDeviceID, RuntimeDone: ctx.Done()})
	if err != nil {
		t.Fatal(err)
	}
	leases := &sessionLeaseClient{}
	manager, err := NewDispatchManager(DispatchManagerConfig{MachineID: f.Dispatch.OwnerDeviceID, Leases: leases, Carriers: &dispatchResolver{carrier: carrier}, Readiness: &dispatchObserver{}, Owners: owners, Now: func() time.Time { return now }, RunContext: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	changed := f.Dispatch
	changed.OwnerSessionID = "another_owner_session"
	if _, err = manager.Dispatch(ctx, authorization, changed); !errors.Is(err, ErrDispatchInvalid) {
		t.Fatalf("changed session: %v", err)
	}
	if _, err = manager.Dispatch(ctx, authorization, f.Dispatch); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		outcome, e := manager.Dispatch(ctx, authorization, f.Dispatch)
		if e != nil {
			t.Fatal(e)
		}
		if outcome.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shared preview did not reach carrier readiness")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stream, err := pair.edge.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = connectorprotocol.WriteStreamOpen(stream, testPreviewStreamOpen(identity, f.Dispatch.PreviewID, "shared_origin_request")); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(stream, "GET / HTTP/1.1\r\nHost: preview.example.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(stream), nil)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	stream.Close()
	if err != nil || string(contents) != "shared-preview-origin" {
		t.Fatal("shared carrier did not reach exact origin")
	}
	if err = manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	remaining := len(manager.operations)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatal("shared owner shutdown retained preview operations")
	}
	if f.Renewed.Preview.AccountID != f.Dispatch.AccountID || f.Stopped.Preview.AccountID != f.Dispatch.AccountID || f.Stopped.Preview.State != "stopped" {
		t.Fatal("server lifecycle lost resource identity")
	}
}

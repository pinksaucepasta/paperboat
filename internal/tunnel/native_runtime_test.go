package tunnel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type rotatingNativeAuth struct {
	mu    sync.Mutex
	token string
}

func (a *rotatingNativeAuth) Credential() (config.Credential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return config.Credential{AccessToken: a.token}, nil
}

func TestCLINativeNetworkAPIUsesCurrentCredential(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = append(seen, request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/peer-network/register" {
			_, _ = writer.Write([]byte(`{"data":{"key_generation":1,"virtual_address":"fd7a:115c:a1e0::1"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"data":{"configuration":"token","candidate_set":"","relay_grants":[]}}`))
	}))
	defer server.Close()
	auth := &rotatingNativeAuth{token: "first"}
	network := cliNativeNetworkAPI{issuer: server.URL, auth: auth, http: server.Client()}
	if _, err := network.RegisterPeerNetwork(t.Context(), api.PeerNetworkRegistration{OperationID: "operation_1"}); err != nil {
		t.Fatal(err)
	}
	auth.mu.Lock()
	auth.token = "second"
	auth.mu.Unlock()
	if _, err := network.PeerNetworkConfiguration(t.Context(), "operation_2"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("authorization headers = %#v", seen)
	}
}

type capturedNativeApplicationSession struct {
	mu           sync.Mutex
	headers      []streamauth.Header
	resources    []string
	capabilities []string
	peers        []net.Conn
	closed       bool
}

func (s *capturedNativeApplicationSession) OpenAuthorized(_ context.Context, header streamauth.Header, resource, capability string) (net.Conn, error) {
	client, peer := net.Pipe()
	s.mu.Lock()
	s.headers = append(s.headers, header)
	s.resources = append(s.resources, resource)
	s.capabilities = append(s.capabilities, capability)
	s.peers = append(s.peers, peer)
	s.mu.Unlock()
	return client, nil
}

func (s *capturedNativeApplicationSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, peer := range s.peers {
		_ = peer.Close()
	}
	return nil
}

func TestCLINativeStreamGroupAuthorizesEveryApplicationStream(t *testing.T) {
	now := time.Now().UTC()
	session := &capturedNativeApplicationSession{}
	target := &resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: "operation-token", ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ResourceID: "access_session_1"}}
	group := &cliNativeStreamGroup{session: session, target: target, application: peerApplication{operationID: "operation_1"}, consumer: "ssh", now: func() time.Time { return now }}
	first, err := group.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := group.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	_ = second.Close()
	_ = group.Close()
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.headers) != 2 || session.headers[0].StreamID == session.headers[1].StreamID {
		t.Fatalf("headers = %#v", session.headers)
	}
	for i, header := range session.headers {
		if header.Consumer != "ssh" || header.OperationID != "operation_1" || !header.Resumable || session.resources[i] != "access_session_1" || session.capabilities[i] != "terminal" {
			t.Fatalf("stream %d header=%#v resource=%q capability=%q", i, header, session.resources[i], session.capabilities[i])
		}
	}
}

type failingNativeApplicationSession struct{ closed bool }

func (*failingNativeApplicationSession) OpenAuthorized(context.Context, streamauth.Header, string, string) (net.Conn, error) {
	return nil, net.ErrClosed
}
func (s *failingNativeApplicationSession) Close() error { s.closed = true; return nil }

func TestCLINativeTransferLeaseRedialsFailedAssociation(t *testing.T) {
	now := time.Now().UTC()
	failed := &failingNativeApplicationSession{}
	replacement := &capturedNativeApplicationSession{}
	var dials int
	lease := &cliNativeTransferLease{
		dial: func(context.Context) (nativeApplicationSession, error) {
			dials++
			if dials == 1 {
				return failed, nil
			}
			return replacement, nil
		},
		target:      &resolver.TerminalTarget{Auth: resolver.AuthTarget{Token: "file-token", ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), ResourceID: "access_1"}},
		application: peerApplication{operationID: "operation_1"}, now: func() time.Time { return now },
	}
	stream, err := lease.OpenTransferStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if dials != 2 || !failed.closed {
		t.Fatalf("dials=%d failed_closed=%t", dials, failed.closed)
	}
	_ = stream.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	if !replacement.closed || len(replacement.headers) != 1 || replacement.headers[0].Consumer != "file_transfer" || replacement.resources[0] != "access_1" || replacement.capabilities[0] != "file_transfer" {
		t.Fatalf("replacement closed=%t headers=%#v resources=%#v capabilities=%#v", replacement.closed, replacement.headers, replacement.resources, replacement.capabilities)
	}
}

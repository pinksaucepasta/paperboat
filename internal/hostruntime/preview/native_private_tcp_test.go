package preview

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type nativePrivateGrantIssuerFunc func(context.Context, api.NativePrivateGrantRequest) (api.NativePrivateGrant, error)

func (f nativePrivateGrantIssuerFunc) IssueNativePrivateGrant(ctx context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
	return f(ctx, request)
}

func TestNativePrivateTCPAccessClassifiesControlPlaneDenial(t *testing.T) {
	access, err := NewNativePrivateTCPAccess(NativePrivateTCPAccessConfig{
		Grants: nativePrivateGrantIssuerFunc(func(context.Context, api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
			return api.NativePrivateGrant{}, &api.APIError{Status: 403, Code: "access_forbidden"}
		}),
		DialSession: func(context.Context, string) (NativePrivateSession, error) { return nil, errors.New("must not dial") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = access.Resolve(context.Background(), "database"); !errors.Is(err, ErrPrivateTCPAccessForbidden) {
		t.Fatalf("resolve error=%v, want definitive denial", err)
	}
}

type nativePrivateTestSession struct {
	mu       sync.Mutex
	accesses []string
}

func (s *nativePrivateTestSession) OpenAuthorized(_ context.Context, header streamauth.Header, access, capability string) (net.Conn, error) {
	s.mu.Lock()
	s.accesses = append(s.accesses, access+":"+capability+":"+header.Consumer+":"+header.OperationID)
	s.mu.Unlock()
	client, host := net.Pipe()
	go func() { defer host.Close(); _, _ = host.Write([]byte{0}); _, _ = io.Copy(host, host) }()
	return client, nil
}
func (*nativePrivateTestSession) Close() error { return nil }

func TestNativePrivateTCPAccessFreshGrantPerConnection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	requests := make([]api.NativePrivateGrantRequest, 0, 4)
	var requestsMu sync.Mutex
	issuer := nativePrivateGrantIssuerFunc(func(_ context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
		requestsMu.Lock()
		requests = append(requests, request)
		requestsMu.Unlock()
		var grant api.NativePrivateGrant
		grant.Target.AccountID, grant.Target.UserID, grant.Target.EnvironmentID = "usr_1", "usr_1", "env_1"
		grant.Target.MachineID, grant.Target.AccessSessionID = "machine_1", "umas_1"
		grant.Target.ResourceKind, grant.Target.ResourceID, grant.Target.ResourceGeneration = "tunnel", "tun_1", 2
		grant.Target.RouteID, grant.Target.RouteGeneration, grant.Target.TargetGeneration = "route_1", 3, 4
		grant.Target.Protocol, grant.Target.TargetScheme, grant.Target.TargetAddress = "tcp", "tcp", "127.0.0.1:5432"
		grant.Credential, grant.ExpiresAt = "credential", now.Add(time.Minute)
		return grant, nil
	})
	session := &nativePrivateTestSession{}
	access, err := NewNativePrivateTCPAccess(NativePrivateTCPAccessConfig{Grants: issuer, DialSession: func(context.Context, string) (NativePrivateSession, error) { return session, nil }, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	routeID, tunnelID, err := access.Resolve(context.Background(), "database")
	if err != nil || routeID != "route_1" || tunnelID != "tun_1" {
		t.Fatalf("route=%q tunnel=%q err=%v", routeID, tunnelID, err)
	}
	proxy, err := access.Start(context.Background(), PrivateTCPAccessRequest{RouteID: routeID, ListenAddress: "127.0.0.1:0", MaximumConnections: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(proxy.AccessURL(), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = connection.Write([]byte("ssh")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 3)
	if _, err = io.ReadFull(connection, response); err != nil || string(response) != "ssh" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	_ = connection.Close()
	requestsMu.Lock()
	requestCount := len(requests)
	issued := append([]api.NativePrivateGrantRequest(nil), requests...)
	requestsMu.Unlock()
	if requestCount != 3 {
		t.Fatalf("grant requests=%d, want selector, preflight, connection", requestCount)
	}
	session.mu.Lock()
	opened := append([]string(nil), session.accesses...)
	session.mu.Unlock()
	prefix := "umas_1:private_access:private_tcp:"
	if len(opened) != 2 || !strings.HasPrefix(opened[0], prefix) || !strings.HasPrefix(opened[1], prefix) || strings.TrimPrefix(opened[0], prefix) != issued[1].OperationID || strings.TrimPrefix(opened[1], prefix) != issued[2].OperationID {
		t.Fatalf("opened=%v issued=%v", opened, issued)
	}
}

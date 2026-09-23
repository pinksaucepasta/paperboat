package preview

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

type nativePrivateHTTPRoutesFunc func(context.Context) ([]NativePrivateHTTPRoute, error)

func (f nativePrivateHTTPRoutesFunc) SnapshotNativePrivateHTTP(ctx context.Context) ([]NativePrivateHTTPRoute, error) {
	return f(ctx)
}

type nativePrivateHTTPRoundTripper func(*http.Request) (*http.Response, error)

func (f nativePrivateHTTPRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type nativePrivateHTTPTestSession struct {
	mu      sync.Mutex
	closed  int
	headers []streamauth.Header
}

func (s *nativePrivateHTTPTestSession) OpenAuthorizedHTTP3(_ context.Context, header streamauth.Header, access string) (http.RoundTripper, error) {
	if access != "umas_1" {
		return nil, ErrPrivateAccessInvalid
	}
	s.mu.Lock()
	s.headers = append(s.headers, header)
	s.mu.Unlock()
	return nativePrivateHTTPRoundTripper(func(request *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			payload, err := io.ReadAll(request.Body)
			if err == nil {
				_, err = writer.Write(append([]byte("origin:"), payload...))
			}
			_ = writer.CloseWithError(err)
		}()
		return &http.Response{StatusCode: http.StatusOK, Body: reader}, nil
	}), nil
}

func (s *nativePrivateHTTPTestSession) Close() error {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return nil
}

func TestNativePrivateHTTPAccessUsesFreshGrantAndSessionWithoutReplay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issued := 0
	issuer := nativePrivateGrantIssuerFunc(func(_ context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
		issued++
		if request.ResourceKind != "preview" || request.ResourceID != "prv_1" || request.RouteID != "prv_1" || request.Protocol != "http" {
			t.Fatalf("request=%+v", request)
		}
		var grant api.NativePrivateGrant
		grant.Target.AccountID, grant.Target.UserID, grant.Target.EnvironmentID = "usr_1", "usr_1", "env_1"
		grant.Target.MachineID, grant.Target.AccessSessionID = "machine_1", "umas_1"
		grant.Target.ResourceKind, grant.Target.ResourceID, grant.Target.ResourceGeneration = "preview", "prv_1", 2
		grant.Target.RouteID, grant.Target.RouteGeneration, grant.Target.TargetGeneration = "prv_1", 2, 2
		grant.Target.Protocol, grant.Target.TargetScheme, grant.Target.TargetAddress = "http", "http", "127.0.0.1:3000"
		grant.Credential, grant.ExpiresAt = "credential", now.Add(time.Minute)
		return grant, nil
	})
	routes := nativePrivateHTTPRoutesFunc(func(context.Context) ([]NativePrivateHTTPRoute, error) {
		return []NativePrivateHTTPRoute{{MatchType: "exact", Hostname: "preview.example.test", ResourceKind: "preview", ResourceID: "prv_1", RouteID: "prv_1"}}, nil
	})
	var sessions []*nativePrivateHTTPTestSession
	access, err := NewNativePrivateHTTPAccess(NativePrivateHTTPAccessConfig{Routes: routes, Grants: issuer, DialSession: func(context.Context, string) (NativePrivateHTTPSession, error) {
		session := &nativePrivateHTTPTestSession{}
		sessions = append(sessions, session)
		return session, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"POST /one\r\n\r\ncount=1", "POST /two\r\n\r\ncount=2"} {
		connection, openErr := access.Open(context.Background(), "preview.example.test")
		if openErr != nil {
			t.Fatal(openErr)
		}
		if _, openErr = connection.Write([]byte(payload)); openErr != nil {
			t.Fatal(openErr)
		}
		_ = connection.(interface{ CloseWrite() error }).CloseWrite()
		response, readErr := io.ReadAll(connection)
		if readErr != nil || string(response) != "origin:"+payload {
			t.Fatalf("response=%q err=%v", response, readErr)
		}
		_ = connection.Close()
	}
	if issued != 2 || len(sessions) != 2 {
		t.Fatalf("grants=%d sessions=%d", issued, len(sessions))
	}
	for _, session := range sessions {
		session.mu.Lock()
		if session.closed != 1 || len(session.headers) != 1 || session.headers[0].Consumer != "private_http" || !strings.Contains(session.headers[0].Target, `"resource_id":"prv_1"`) {
			t.Fatalf("closed=%d headers=%+v", session.closed, session.headers)
		}
		session.mu.Unlock()
	}
}

package preview

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
	if _, err = access.DialMachine(context.Background(), "machine_1", 5432); !errors.Is(err, ErrPrivateTCPAccessForbidden) {
		t.Fatalf("resolve error=%v, want definitive denial", err)
	}
	var original *api.APIError
	if !errors.As(err, &original) || original.Status != 403 {
		t.Fatal("grant denial discarded the original HTTP cause")
	}
	fault := errorreport.ProjectFault(t.Context(), "paperboat-daemon", "local_gateway", "local_gateway", "local_gateway_failed", err)
	if fault.Stage != "grant_issue" || fault.HTTPStatus != 403 || fault.Cause != "permission_denied" {
		t.Fatalf("grant denial cannot be diagnosed: %#v", fault)
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

func machineGrant(now time.Time) api.NativePrivateGrant {
	var g api.NativePrivateGrant
	g.Target.AccountID = "owner"
	g.Target.UserID = "owner"
	g.Target.EnvironmentID = "env"
	g.Target.MachineID = "machine"
	g.Target.AccessSessionID = "access"
	g.Target.CLIClientSessionID = "cli"
	g.Target.ResourceKind = "machine_service"
	g.Target.ResourceID = "machine"
	g.Target.ResourceGeneration = 1
	g.Target.RouteID = "tcp:5432"
	g.Target.RouteGeneration = 2
	g.Target.TargetGeneration = 3
	g.Target.Protocol = "tcp"
	g.Target.TargetScheme = "tcp"
	g.Target.TargetAddress = "127.0.0.1:5432"
	g.Target.InstallationGeneration = 1
	g.Target.BootID = "boot"
	g.Target.PolicyGeneration = 2
	g.Target.AnnouncementGeneration = 3
	g.Credential = "credential"
	g.ExpiresAt = now.Add(time.Minute)
	return g
}
func TestNativePrivateMachineGrantBeforeDialAndExactTarget(t *testing.T) {
	for _, scenario := range []string{"denied", "wrong_machine", "wrong_port", "missing_fence", "success"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			dials := 0
			requests := 0
			access, err := NewNativePrivateTCPAccess(NativePrivateTCPAccessConfig{Grants: nativePrivateGrantIssuerFunc(func(_ context.Context, r api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
				requests++
				if r.ResourceKind != "machine_service" || r.ResourceID != "machine" || r.RouteID != "tcp:5432" || r.Protocol != "tcp" || r.OperationID == "" {
					t.Fatalf("wrong request: %+v", r)
				}
				g := machineGrant(now)
				switch scenario {
				case "denied":
					return g, &api.APIError{Status: 403}
				case "wrong_machine":
					g.Target.MachineID = "foreign"
				case "wrong_port":
					g.Target.RouteID = "tcp:5433"
					g.Target.TargetAddress = "127.0.0.1:5433"
				case "missing_fence":
					g.Target.AnnouncementGeneration = 0
				}
				return g, nil
			}), DialSession: func(context.Context, string) (NativePrivateSession, error) {
				dials++
				return &nativePrivateTestSession{}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			conn, err := access.DialMachine(t.Context(), "machine", 5432)
			if scenario != "success" {
				if err == nil || dials != 0 {
					t.Fatalf("unauthorized dial: %v %d", err, dials)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if requests != 1 || dials != 1 {
				t.Fatal("grant ownership")
			}
			if _, err = conn.Write([]byte("private bytes")); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, 13)
			if _, err = io.ReadFull(conn, reply); err != nil || string(reply) != "private bytes" {
				t.Fatalf("reply %q %v", reply, err)
			}
		})
	}
}

type blockedPrivateSession struct {
	host   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (s *blockedPrivateSession) OpenAuthorized(context.Context, streamauth.Header, string, string) (net.Conn, error) {
	client, host := net.Pipe()
	s.host = host
	return client, nil
}
func (s *blockedPrivateSession) Close() error {
	s.once.Do(func() {
		if s.host != nil {
			_ = s.host.Close()
		}
		close(s.closed)
	})
	return nil
}
func TestNativePrivateMachineCancellationClosesPendingReadiness(t *testing.T) {
	session := &blockedPrivateSession{closed: make(chan struct{})}
	started := make(chan struct{})
	access, err := NewNativePrivateTCPAccess(NativePrivateTCPAccessConfig{Grants: nativePrivateGrantIssuerFunc(func(context.Context, api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
		return machineGrant(time.Now()), nil
	}), DialSession: func(context.Context, string) (NativePrivateSession, error) { close(started); return session, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := access.DialMachine(ctx, "machine", 5432); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("canceled readiness lost its cancellation cause")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt readiness")
	}
	select {
	case <-session.closed:
	case <-time.After(time.Second):
		t.Fatal("session leaked")
	}
}

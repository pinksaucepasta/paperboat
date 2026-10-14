package preview

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

// nativePrivateTCPFailure keeps the exact failed stage and original cause while
// presenting the existing safe access error to callers.
type nativePrivateTCPFailure struct {
	stage  string
	public error
	cause  error
}

func (e *nativePrivateTCPFailure) Error() string           { return e.public.Error() }
func (e *nativePrivateTCPFailure) Unwrap() error           { return e.cause }
func (e *nativePrivateTCPFailure) Is(target error) bool    { return target == e.public }
func (e *nativePrivateTCPFailure) DiagnosticStage() string { return e.stage }

func nativePrivateTCPFailed(ctx context.Context, stage string, err error) error {
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	public := mapNativePrivateTCPAccessError(err)
	if errors.Is(err, context.Canceled) {
		public = context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		public = context.DeadlineExceeded
	}
	return &nativePrivateTCPFailure{stage: stage, public: public, cause: err}
}

type NativePrivateGrantIssuer interface {
	IssueNativePrivateGrant(context.Context, api.NativePrivateGrantRequest) (api.NativePrivateGrant, error)
}

type NativePrivateTCPAccessConfig struct {
	Grants       NativePrivateGrantIssuer
	DialSession  func(context.Context, string) (NativePrivateSession, error)
	Now          func() time.Time
	MaximumBytes uint64
}

type NativePrivateSession interface {
	OpenAuthorized(context.Context, streamauth.Header, string, string) (net.Conn, error)
	Close() error
}

// NativePrivateTCPAccess is injectable into stable hostd. It owns no network
// engine: Task 20 supplies the one shared native session owner.
type NativePrivateTCPAccess struct{ config NativePrivateTCPAccessConfig }

func NewNativePrivateTCPAccess(config NativePrivateTCPAccessConfig) (*NativePrivateTCPAccess, error) {
	if config.Grants == nil || config.DialSession == nil {
		return nil, ErrPrivateTCPClientInvalid
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.MaximumBytes == 0 {
		config.MaximumBytes = 1 << 40
	}
	return &NativePrivateTCPAccess{config: config}, nil
}

func (a *NativePrivateTCPAccess) openGrant(ctx context.Context, operationID string, grant api.NativePrivateGrant) (net.Conn, error) {
	binding, err := grant.Binding()
	if err != nil {
		return nil, nativePrivateTCPFailed(ctx, "grant_issue", err)
	}
	header, err := streamauth.NewNativePrivate(operationID, "private_tcp", nativePrivateOperationID(), grant.Credential, grant.ExpiresAt, a.config.MaximumBytes, binding)
	if err != nil {
		return nil, nativePrivateTCPFailed(ctx, "grant_issue", err)
	}
	session, err := a.config.DialSession(ctx, grant.Target.MachineID)
	if err != nil {
		return nil, nativePrivateTCPFailed(ctx, "peer_connect", err)
	}
	connection, err := session.OpenAuthorized(ctx, header, grant.Target.AccessSessionID, "private_access")
	if err != nil {
		_ = session.Close()
		return nil, nativePrivateTCPFailed(ctx, "stream_open", err)
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = session.Close() })
	defer stopCancel()
	deadline := a.config.Now().Add(10 * time.Second)
	if grant.ExpiresAt.Before(deadline) {
		deadline = grant.ExpiresAt
	}
	if err = connection.SetReadDeadline(deadline); err != nil {
		_ = connection.Close()
		_ = session.Close()
		return nil, nativePrivateTCPFailed(ctx, "target_connect", err)
	}
	var ready [1]byte
	_, err = io.ReadFull(connection, ready[:])
	err = errors.Join(err, connection.SetReadDeadline(time.Time{}))
	if err != nil || ready[0] != 0 {
		_ = connection.Close()
		_ = session.Close()
		if ctx.Err() != nil {
			err = errors.Join(ctx.Err(), err)
		}
		if err == nil {
			err = ErrPrivateTCPClientUnavailable
		}
		return nil, nativePrivateTCPFailed(ctx, "target_connect", err)
	}
	if ctx.Err() != nil {
		_ = connection.Close()
		_ = session.Close()
		return nil, nativePrivateTCPFailed(ctx, "target_connect", ctx.Err())
	}
	return &nativePrivateTCPConnection{Conn: connection, session: session}, nil
}

func mapNativePrivateTCPAccessError(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized:
			return ErrPrivateTCPAccessAuthentication
		case http.StatusForbidden, http.StatusNotFound:
			return ErrPrivateTCPAccessForbidden
		}
	}
	return mapPrivateTCPAccessError(err)
}

func (a *NativePrivateTCPAccess) issue(ctx context.Context, request api.NativePrivateGrantRequest) (api.NativePrivateGrant, error) {
	if a == nil || a.config.Grants == nil {
		return api.NativePrivateGrant{}, ErrPrivateTCPClientInvalid
	}
	return a.config.Grants.IssueNativePrivateGrant(ctx, request)
}

func nativePrivateOperationID() string {
	return "operation_" + uuid.NewString()
}

type nativePrivateTCPConnection struct {
	net.Conn
	session  NativePrivateSession
	once     sync.Once
	closeErr error
}

func (c *nativePrivateTCPConnection) Close() error {
	c.once.Do(func() { c.closeErr = errors.Join(c.Conn.Close(), c.session.Close()) })
	return c.closeErr
}

// DialMachine obtains a fresh exact-port grant before consulting network reachability.
func (a *NativePrivateTCPAccess) DialMachine(ctx context.Context, machineID string, port int) (net.Conn, error) {
	if a == nil || ctx == nil || machineID == "" || port < 1 || port > 65535 {
		return nil, ErrPrivateTCPClientInvalid
	}
	operation := nativePrivateOperationID()
	route := "tcp:" + strconv.Itoa(port)
	grant, err := a.issue(ctx, api.NativePrivateGrantRequest{OperationID: operation, ResourceKind: "machine_service", ResourceID: machineID, RouteID: route, Protocol: "tcp"})
	if err != nil {
		return nil, nativePrivateTCPFailed(ctx, "grant_issue", err)
	}
	raw, err := grant.Binding()
	if err != nil {
		return nil, nativePrivateTCPFailed(ctx, "grant_issue", err)
	}
	binding, err := nativeprivate.Decode(raw, a.config.Now())
	if err != nil || binding.ResourceKind != "machine_service" || binding.ResourceID != machineID || binding.OwnerEndpointID != machineID || binding.RouteID != route || grant.Credential == "" || grant.Target.AccessSessionID == "" {
		return nil, &nativePrivateTCPFailure{stage: "grant_issue", public: ErrPrivateTCPAccessForbidden, cause: errors.Join(ErrPrivateTCPAccessForbidden, err)}
	}
	return a.openGrant(ctx, operation, grant)
}

func (c *nativePrivateTCPConnection) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return c.Close()
}

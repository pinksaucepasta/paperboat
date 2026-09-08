package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

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
type NativePrivateTCPAccess struct {
	config  NativePrivateTCPAccessConfig
	mu      sync.RWMutex
	targets map[string]nativePrivateTCPState
}

type nativePrivateTCPState struct {
	tunnelID, machineID, address                          string
	resourceGeneration, routeGeneration, targetGeneration uint64
}

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
	return &NativePrivateTCPAccess{config: config, targets: make(map[string]nativePrivateTCPState)}, nil
}

func (a *NativePrivateTCPAccess) Resolve(ctx context.Context, selector string) (string, string, error) {
	grant, err := a.issue(ctx, api.NativePrivateGrantRequest{OperationID: nativePrivateOperationID(), Selector: selector})
	if err != nil {
		return "", "", mapNativePrivateTCPAccessError(err)
	}
	a.mu.Lock()
	a.targets[grant.Target.RouteID] = nativePrivateTCPState{tunnelID: grant.Target.ResourceID, machineID: grant.Target.MachineID, address: grant.Target.TargetAddress, resourceGeneration: grant.Target.ResourceGeneration, routeGeneration: grant.Target.RouteGeneration, targetGeneration: grant.Target.TargetGeneration}
	a.mu.Unlock()
	return grant.Target.RouteID, grant.Target.ResourceID, nil
}

func (a *NativePrivateTCPAccess) Start(ctx context.Context, request PrivateTCPAccessRequest) (privateTCPAccessProxy, error) {
	if a == nil || ctx == nil || request.RouteID == "" {
		return nil, ErrPrivateTCPClientInvalid
	}
	proxy, err := privatepreviewproxy.Start(ctx, privatepreviewproxy.Config{ListenPort: request.ListenPort, ListenAddress: request.ListenAddress, MaximumConnections: request.MaximumConnections, Dial: func(openCtx context.Context) (io.ReadWriteCloser, error) {
		return a.open(openCtx, request.RouteID)
	}})
	if err != nil {
		return nil, err
	}
	return machinePrivateTCPProxy{proxy: proxy}, nil
}

func (a *NativePrivateTCPAccess) open(ctx context.Context, routeID string) (io.ReadWriteCloser, error) {
	a.mu.RLock()
	state := a.targets[routeID]
	a.mu.RUnlock()
	if state.tunnelID == "" {
		return nil, ErrPrivateTCPClientUnavailable
	}
	operationID := nativePrivateOperationID()
	grant, err := a.issue(ctx, api.NativePrivateGrantRequest{OperationID: operationID, ResourceKind: "tunnel", ResourceID: state.tunnelID, RouteID: routeID, Protocol: "tcp"})
	if err != nil {
		return nil, err
	}
	binding, err := grant.Binding()
	if err != nil {
		return nil, err
	}
	session, err := a.config.DialSession(ctx, grant.Target.MachineID)
	if err != nil {
		return nil, err
	}
	header, err := streamauth.NewNativePrivate(operationID, "private_tcp", nativePrivateOperationID(), grant.Credential, grant.ExpiresAt, a.config.MaximumBytes, binding)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	connection, err := session.OpenAuthorized(ctx, header, grant.Target.AccessSessionID, "private_access")
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	deadline := a.config.Now().Add(10 * time.Second)
	if grant.ExpiresAt.Before(deadline) {
		deadline = grant.ExpiresAt
	}
	_ = connection.SetReadDeadline(deadline)
	var ready [1]byte
	_, err = io.ReadFull(connection, ready[:])
	_ = connection.SetReadDeadline(time.Time{})
	if err != nil || ready[0] != 0 {
		_ = connection.Close()
		_ = session.Close()
		return nil, ErrPrivateTCPClientUnavailable
	}
	return &nativePrivateTCPConnection{Conn: connection, session: session}, nil
}

func (a *NativePrivateTCPAccess) Validate(ctx context.Context, tunnelID, routeID string) (time.Time, error) {
	a.mu.RLock()
	state := a.targets[routeID]
	a.mu.RUnlock()
	if state.tunnelID == "" || state.tunnelID != tunnelID {
		return time.Time{}, ErrPrivateTCPAccessForbidden
	}
	grant, err := a.issue(ctx, api.NativePrivateGrantRequest{OperationID: nativePrivateOperationID(), ResourceKind: "tunnel", ResourceID: tunnelID, RouteID: routeID, Protocol: "tcp"})
	if err != nil {
		return time.Time{}, mapNativePrivateTCPAccessError(err)
	}
	if grant.Target.MachineID != state.machineID || grant.Target.TargetAddress != state.address || grant.Target.ResourceGeneration != state.resourceGeneration || grant.Target.RouteGeneration != state.routeGeneration || grant.Target.TargetGeneration != state.targetGeneration {
		return time.Time{}, ErrPrivateTCPAccessForbidden
	}
	return grant.ExpiresAt, nil
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
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		return "native_private_unavailable"
	}
	return "native_private_" + hex.EncodeToString(value[:])
}

type nativePrivateTCPConnection struct {
	net.Conn
	session NativePrivateSession
}

func (c *nativePrivateTCPConnection) Close() error {
	return errors.Join(c.Conn.Close(), c.session.Close())
}

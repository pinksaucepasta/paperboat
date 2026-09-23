package preview

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
)

type NativePrivateHTTPRoute struct {
	MatchType, Hostname, ResourceKind, ResourceID, RouteID string
}

type NativePrivateHTTPRoutes interface {
	SnapshotNativePrivateHTTP(context.Context) ([]NativePrivateHTTPRoute, error)
}

type NativePrivateHTTPSession interface {
	OpenAuthorizedHTTP3(context.Context, streamauth.Header, string) (http.RoundTripper, error)
	Close() error
}

type NativePrivateHTTPAccessConfig struct {
	Routes       NativePrivateHTTPRoutes
	Grants       NativePrivateGrantIssuer
	DialSession  func(context.Context, string) (NativePrivateHTTPSession, error)
	MaximumBytes uint64
}

// NativePrivateHTTPAccess supplies the existing PAC/CONNECT listener without
// edge admissions or an ActiveDataCarrier. Each CONNECT owns a new authorized
// preview-class native HTTP/3 session.
type NativePrivateHTTPAccess struct{ config NativePrivateHTTPAccessConfig }

func NewNativePrivateHTTPAccess(config NativePrivateHTTPAccessConfig) (*NativePrivateHTTPAccess, error) {
	if config.Routes == nil || config.Grants == nil || config.DialSession == nil {
		return nil, ErrPrivateAccessInvalid
	}
	if config.MaximumBytes == 0 {
		config.MaximumBytes = 1 << 30
	}
	return &NativePrivateHTTPAccess{config: config}, nil
}

func (a *NativePrivateHTTPAccess) Snapshot(ctx context.Context) ([]privatepreviewproxy.AccessRoute, error) {
	routes, err := a.config.Routes.SnapshotNativePrivateHTTP(ctx)
	if err != nil {
		return nil, mapNativePrivateTCPAccessError(err)
	}
	result := make([]privatepreviewproxy.AccessRoute, 0, len(routes))
	for _, route := range routes {
		if route.Hostname == "" || route.ResourceID == "" || route.RouteID == "" || route.ResourceKind != "preview" && route.ResourceKind != "tunnel" {
			return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
		}
		result = append(result, privatepreviewproxy.AccessRoute{MatchType: route.MatchType, Hostname: route.Hostname})
	}
	return result, nil
}

func (a *NativePrivateHTTPAccess) Open(ctx context.Context, host string) (io.ReadWriteCloser, error) {
	routes, err := a.config.Routes.SnapshotNativePrivateHTTP(ctx)
	if err != nil {
		return nil, mapNativePrivateTCPAccessError(err)
	}
	var selected *NativePrivateHTTPRoute
	for i := range routes {
		matches := routes[i].Hostname == host
		if routes[i].MatchType == privatepreviewproxy.AccessMatchOneLabelWildcard {
			suffix := strings.TrimPrefix(routes[i].Hostname, "*.")
			prefix, ok := strings.CutSuffix(host, "."+suffix)
			matches = ok && prefix != "" && !strings.Contains(prefix, ".")
		}
		if matches {
			if selected != nil {
				return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
			}
			selected = &routes[i]
		}
	}
	if selected == nil {
		return nil, privatepreviewproxy.ErrAccessForbidden
	}
	operationID := nativePrivateOperationID()
	grant, err := a.config.Grants.IssueNativePrivateGrant(ctx, api.NativePrivateGrantRequest{OperationID: operationID, ResourceKind: selected.ResourceKind, ResourceID: selected.ResourceID, RouteID: selected.RouteID, Protocol: "http"})
	if err != nil {
		return nil, mapNativePrivateTCPAccessError(err)
	}
	binding, err := grant.Binding()
	if err != nil {
		return nil, err
	}
	session, err := a.config.DialSession(ctx, grant.Target.MachineID)
	if err != nil {
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	header, err := streamauth.NewNativePrivate(operationID, "private_http", nativePrivateOperationID(), grant.Credential, grant.ExpiresAt, a.config.MaximumBytes, binding)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	transport, err := session.OpenAuthorizedHTTP3(ctx, header, grant.Target.AccessSessionID)
	if err != nil {
		_ = session.Close()
		return nil, errors.Join(privatepreviewproxy.ErrAccessTemporarilyUnavailable, err)
	}
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	reader, writer := io.Pipe()
	request := (&http.Request{Method: http.MethodConnect, URL: &url.URL{Scheme: "https", Host: "private-http.paperboat", Path: "/"}, Proto: nativeprivate.HTTP3ConnectProtocol, Host: "private-http.paperboat", Header: make(http.Header), Body: reader}).WithContext(streamCtx)
	encodedHeader, err := header.MarshalBinary()
	if err != nil {
		cancel()
		_ = writer.Close()
		_ = session.Close()
		return nil, privatepreviewproxy.ErrAccessTemporarilyUnavailable
	}
	request.Header.Set("X-Paperboat-Native-Authorization", base64.RawURLEncoding.EncodeToString(encodedHeader))
	type outcome struct {
		response *http.Response
		err      error
	}
	ready := make(chan outcome, 1)
	go func() { response, roundErr := transport.RoundTrip(request); ready <- outcome{response, roundErr} }()
	select {
	case result := <-ready:
		if result.err != nil || result.response.StatusCode != http.StatusOK {
			if result.response != nil {
				_ = result.response.Body.Close()
			}
			cancel()
			_ = writer.Close()
			_ = session.Close()
			return nil, errors.Join(privatepreviewproxy.ErrAccessTemporarilyUnavailable, result.err)
		}
		return &nativePrivateHTTPConnection{reader: result.response.Body, writer: writer, cancel: cancel, session: session}, nil
	case <-ctx.Done():
		cancel()
		_ = writer.CloseWithError(ctx.Err())
		_ = session.Close()
		return nil, ctx.Err()
	}
}

type nativePrivateHTTPConnection struct {
	reader  io.ReadCloser
	writer  *io.PipeWriter
	cancel  context.CancelFunc
	session NativePrivateHTTPSession
	once    sync.Once
	err     error
}

func (c *nativePrivateHTTPConnection) Read(value []byte) (int, error)  { return c.reader.Read(value) }
func (c *nativePrivateHTTPConnection) Write(value []byte) (int, error) { return c.writer.Write(value) }
func (c *nativePrivateHTTPConnection) CloseWrite() error               { return c.writer.Close() }
func (c *nativePrivateHTTPConnection) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.err = errors.Join(c.writer.Close(), c.reader.Close(), c.session.Close())
	})
	return c.err
}

var _ privatepreviewproxy.AccessSource = (*NativePrivateHTTPAccess)(nil)

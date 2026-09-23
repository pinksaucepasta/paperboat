package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// InspectorRequest performs one explicit request on an authenticated native
// stream. No HTTP retry or edge fallback is installed, including after an
// ambiguous replay result.
func (t *PeerTerminalTunnel) InspectorRequest(ctx context.Context, access api.InspectorAccess, grant, method, path string, body []byte, query url.Values) ([]byte, int, error) {
	if t == nil || ctx == nil || !strings.HasPrefix(path, "/v1/inspector/") || len(body) > 64<<10 || grant == "" {
		return nil, 0, ErrPeerTerminalInvalid
	}
	credential, err := t.config.Auth.Credential()
	if err != nil {
		return nil, 0, err
	}
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return nil, 0, err
	}
	client := api.New(t.config.Issuer, credential, t.config.HTTPClient)
	identity, err := clientauthority.ResolveLocal(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, Now: t.config.Now().UTC()})
	if err != nil {
		return nil, 0, err
	}
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, identity)
	if !consumed {
		identity.Clear()
	}
	if err != nil {
		return nil, 0, err
	}
	session, err := runtime.Dial(ctx, access.MachineID, peerquic.ClassInteractive)
	if err != nil {
		return nil, 0, err
	}
	defer session.Close()
	action := "inspect"
	if method != http.MethodGet {
		action = "replay"
	}
	target, _ := json.Marshal(map[string]string{"resource_kind": access.ResourceKind, "resource_id": access.ResourceID, "route_id": access.RouteID, "action": action})
	deadline := time.Now().Add(45 * time.Second)
	if access.ExpiresAt.Before(deadline) {
		deadline = access.ExpiresAt
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	header, err := streamauth.NewNativePrivate(access.CredentialID, "inspector", "inspector_request", grant, deadline, (2<<20)+(64<<10), target)
	if err != nil {
		return nil, 0, err
	}
	stream, err := session.OpenAuthorized(ctx, header, access.CredentialID, "inspector")
	if err != nil {
		return nil, 0, err
	}
	defer stream.Close()
	var used atomic.Bool
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		if !used.CompareAndSwap(false, true) {
			return nil, errors.New("inspector stream already used")
		}
		return stream, nil
	}, DisableKeepAlives: true, MaxResponseHeaderBytes: 16 << 10}
	defer transport.CloseIdleConnections()
	u := &url.URL{Scheme: "http", Host: "inspector.paperboat", Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Paperboat-Inspector-Grant", grant)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport, Timeout: time.Until(deadline), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(payload) > 2<<20 {
		return nil, response.StatusCode, errors.New("inspector response unavailable or too large")
	}
	return payload, response.StatusCode, nil
}

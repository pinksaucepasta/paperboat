//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorauth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"net"
	"net/http"
	"strings"
	"time"
)

type nativeInspectorTarget struct {
	Kind     string `json:"resource_kind"`
	Resource string `json:"resource_id"`
	Route    string `json:"route_id"`
	Action   string `json:"action"`
}

func parseNativeInspector(h streamauth.Header) (nativeInspectorTarget, error) {
	var t nativeInspectorTarget
	d := json.NewDecoder(strings.NewReader(h.Target))
	d.DisallowUnknownFields()
	if h.Consumer != "inspector" || h.MaximumBytes > (2<<20)+(64<<10) || d.Decode(&t) != nil || (t.Kind != "preview" && t.Kind != "tunnel") || t.Resource == "" || t.Route == "" || (t.Action != "inspect" && t.Action != "replay") {
		return t, ErrProductionInvalid
	}
	return t, nil
}
func (s *productionNativePeerService) inspectorNetworkAuthorizer() func(context.Context, streamauth.Header) (string, error) {
	ordinary := nativeNetworkAuthorizer(s.config.authorizer)
	authorize, err := (inspectorauth.Config{BaseURL: s.config.controlURL, Source: s.config.identity, Client: &http.Client{Transport: s.config.transport, Timeout: 15 * time.Second}}).AuthorizeFunc()
	return func(ctx context.Context, h streamauth.Header) (string, error) {
		if h.Consumer != "inspector" {
			return ordinary(ctx, h)
		}
		if err != nil || s.config.inspector == nil {
			return "", ErrProductionInvalid
		}
		target, e := parseNativeInspector(h)
		if e != nil {
			return "", e
		}
		d, e := authorize(ctx, h.Credential, target.Kind, target.Resource, target.Route, target.Action)
		if e != nil {
			return "", e
		}
		if d.CredentialID != h.OperationID {
			return "", ErrProductionInvalid
		}
		return d.CredentialID, nil
	}
}
func (s *productionNativePeerService) serveInspector(ctx context.Context, h streamauth.Header, conn net.Conn) error {
	target, err := parseNativeInspector(h)
	if err != nil || s.config.inspector == nil {
		return ErrProductionInvalid
	}
	deadline := time.Unix(h.DeadlineUnix, 0)
	if ceiling := time.Now().Add(45 * time.Second); ceiling.Before(deadline) {
		deadline = ceiling
	}
	_ = conn.SetDeadline(deadline)
	return server.ServeInspectorHTTPConnection(ctx, conn, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		action := "inspect"
		if r.Method != http.MethodGet {
			action = "replay"
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/inspector/") || r.Header.Get("X-Paperboat-Inspector-Grant") != h.Credential || action != target.Action {
			http.Error(w, "inspector access denied", http.StatusForbidden)
			return
		}
		s.config.inspector.ServeHTTP(w, r)
	}))
}

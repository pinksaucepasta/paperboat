package usermachines

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/runtimeattachment"
	"net/url"
	"strings"
)

func (s *Service) runtimeIngressURL(ctx context.Context, environmentID, publicHost string) (string, error) {
	value, err := runtimeattachment.IngressURL(ctx, s.db, environmentID, publicHost)
	if errors.Is(err, runtimeattachment.ErrUnavailable) {
		return "", ErrProvisioningUnavailable
	}
	return value, err
}

// nativeFileTransferEndpoint is the descriptor endpoint a native client streams to: the ingress authority and the
// receiver's own path. A self-hosted ingress URL carries the edge-only /v1/runtime/<host> prefix, which the
// receiver would answer with 404 (the edge strips it for browser and relay traffic, never for a native stream).
func nativeFileTransferEndpoint(httpIngressURL string) string {
	u, err := url.Parse(httpIngressURL)
	if err != nil {
		return httpIngressURL + "/v1/file-transfers"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/v1/file-transfers"}).String()
}

func runtimeWebsocketBase(httpBase string) string {
	return strings.Replace(httpBase, "https://", "wss://", 1)
}

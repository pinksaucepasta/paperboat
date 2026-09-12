// Package inspectorauth calls the control-plane inspector authorize endpoint
// on the daemon's machine-authenticated channel. It carries no authority of
// its own: every decision comes from live server re-resolution.
package inspectorauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/inspectorapi"
)

// MachineSource issues the host's renewable machine identity material,
// mirroring the preview dispatch and config-sync callers.
type MachineSource interface {
	Token(context.Context) (string, error)
	Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error)
}

type Config struct {
	// BaseURL is the control-plane origin (https in production).
	BaseURL string
	Source  MachineSource
	Client  *http.Client
}

type decisionWire struct {
	AccountID          string    `json:"account_id"`
	OwnerAccountID     string    `json:"owner_account_id"`
	ResourceKind       string    `json:"resource_kind"`
	ResourceID         string    `json:"resource_id"`
	RouteID            string    `json:"route_id"`
	ResourceGeneration uint64    `json:"resource_generation"`
	RouteGeneration    uint64    `json:"route_generation"`
	TargetGeneration   uint64    `json:"target_generation"`
	CredentialID       string    `json:"credential_id"`
	IssuedAt           time.Time `json:"issued_at"`
	ExpiresAt          time.Time `json:"expires_at"`
}

// AuthorizeFunc returns an inspectorapi authorizer closing over this client.
// Server denials (403/404/410) become ErrDenied so callers may retry once
// with a freshly issued credential; transport and machine failures become
// ErrUpstream and must surface without an issuance retry.
func (c Config) AuthorizeFunc() (inspectorapi.AuthorizeFunc, error) {
	base := strings.TrimSuffix(strings.TrimSpace(c.BaseURL), "/")
	if base == "" || c.Source == nil {
		return nil, errors.New("invalid inspector authorizer configuration")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return func(ctx context.Context, grantToken, kind, resource, route, action string) (inspectorapi.Decision, error) {
		if ctx == nil || strings.TrimSpace(grantToken) == "" || (kind != "preview" && kind != "tunnel") || strings.TrimSpace(resource) == "" || strings.TrimSpace(route) == "" || (action != "inspect" && action != "replay") {
			return inspectorapi.Decision{}, inspectorapi.ErrInvalid
		}
		body, err := json.Marshal(map[string]any{"credential_token": grantToken, "resource_kind": kind, "resource_id": resource, "route_id": route, "action": action})
		if err != nil {
			return inspectorapi.Decision{}, inspectorapi.ErrInvalid
		}
		const path = "/v1/inspector/authorize"
		operationID, err := newOperationID()
		if err != nil {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		token, err := c.Source.Token(ctx)
		if err != nil || strings.TrimSpace(token) == "" {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		proof, err := c.Source.Proof(ctx, operationID, http.MethodPost, path, body)
		if err != nil || len(proof) == 0 {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(body))
		if err != nil {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Paperboat-Machine-Identity", token)
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
		request.Header.Set("Idempotency-Key", operationID)
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return inspectorapi.Decision{}, ctx.Err()
			}
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		defer response.Body.Close()
		payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if err != nil {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		switch response.StatusCode {
		case http.StatusOK:
		case http.StatusBadRequest:
			return inspectorapi.Decision{}, inspectorapi.ErrInvalid
		case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return inspectorapi.Decision{}, inspectorapi.ErrDenied
		default:
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		var envelope struct {
			Data decisionWire `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return inspectorapi.Decision{}, inspectorapi.ErrUpstream
		}
		wire := envelope.Data
		if wire.CredentialID == "" || wire.AccountID == "" || wire.ResourceKind != kind || wire.ResourceID != resource || wire.RouteID != route ||
			wire.ResourceGeneration == 0 || wire.RouteGeneration == 0 || wire.TargetGeneration == 0 ||
			wire.IssuedAt.IsZero() || wire.ExpiresAt.IsZero() || !wire.ExpiresAt.After(wire.IssuedAt) {
			return inspectorapi.Decision{}, inspectorapi.ErrDenied
		}
		return inspectorapi.Decision{
			Principal: wire.AccountID, Owner: wire.OwnerAccountID, CredentialID: wire.CredentialID,
			ResourceKind: wire.ResourceKind, ResourceID: wire.ResourceID, RouteID: wire.RouteID,
			ResourceGeneration: wire.ResourceGeneration, RouteGeneration: wire.RouteGeneration, TargetGeneration: wire.TargetGeneration,
			IssuedAt: wire.IssuedAt, ExpiresAt: wire.ExpiresAt,
		}, nil
	}, nil
}

func newOperationID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("inspector operation id: %w", err)
	}
	return "iia_" + hex.EncodeToString(value[:]), nil
}

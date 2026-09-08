package tunnelmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type IngressAuthorityFunc func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error)

type IngressMachineAuth interface {
	Token(context.Context) (string, error)
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

// NewBrowserIngressAuthority resolves the exact viewer grant independently of
// the edge, using the host's renewable machine identity and request-bound proof.
func NewBrowserIngressAuthority(controlURL string, auth IngressMachineAuth, transport http.RoundTripper) (IngressAuthorityFunc, error) {
	base, err := url.Parse(controlURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || auth == nil {
		return nil, connectorprotocol.ErrIngressDenied
	}
	endpoint := *base
	endpoint.Path = strings.TrimRight(base.Path, "/") + "/v1/browser-access/ingress/authorize"
	endpoint.RawPath = ""
	client := &http.Client{Transport: transport, Timeout: connectorprotocol.IngressAuthorityLifetime, CheckRedirect: func(*http.Request, []*http.Request) error { return connectorprotocol.ErrIngressDenied }}
	return func(ctx context.Context, open connectorprotocol.StreamOpen, decision connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		denied := connectorprotocol.IngressDecision{}
		// The original claim may have expired while an active stream refreshes.
		// Only the independently returned authority can extend that stream's life.
		if ctx == nil || open.Validate() != nil || decision.Validate(decision.IssuedAt) != nil || decision.Binding.Audience == "public" {
			return denied, connectorprotocol.ErrIngressDenied
		}
		body, err := json.Marshal(struct {
			Decision connectorprotocol.IngressDecision `json:"decision"`
			Open     connectorprotocol.StreamOpen      `json:"open"`
		}{decision, open})
		if err != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return denied, err
		}
		operation := "browser-ingress-" + hex.EncodeToString(nonce[:])
		token, err := auth.Token(ctx)
		if err != nil || strings.TrimSpace(token) == "" {
			return denied, connectorprotocol.ErrIngressDenied
		}
		proof, err := auth.Proof(ctx, operation, http.MethodPost, endpoint.Path, body)
		if err != nil || len(proof) == 0 {
			return denied, connectorprotocol.ErrIngressDenied
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		request.Header.Set("X-Paperboat-Machine-Identity", token)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
		request.Header.Set("Idempotency-Key", operation)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		defer response.Body.Close()
		contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if response.StatusCode != http.StatusOK || err != nil || contentType != "application/json" {
			return denied, connectorprotocol.ErrIngressDenied
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		if err != nil || len(raw) > 64<<10 {
			return denied, connectorprotocol.ErrIngressDenied
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		start, err := decoder.Token()
		if err != nil || start != json.Delim('{') {
			return denied, connectorprotocol.ErrIngressDenied
		}
		key, err := decoder.Token()
		if err != nil || key != "data" {
			return denied, connectorprotocol.ErrIngressDenied
		}
		var payload json.RawMessage
		if decoder.Decode(&payload) != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
			return denied, connectorprotocol.ErrIngressDenied
		}
		// Reuse the strict connector decoder, including duplicate-key rejection.
		var wire bytes.Buffer
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
		wire.Write(length[:])
		wire.Write(payload)
		current, err := connectorprotocol.ReadIngressDecision(&wire, time.Now().UTC())
		if err != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}

		expected := decision
		expected.IssuedAt, expected.ExpiresAt = current.IssuedAt, current.ExpiresAt
		if expected.Authorize(current, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, time.Now().UTC()) != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		return current, nil
	}, nil
}

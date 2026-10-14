package tunnelmanager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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
	if err != nil {
		return nil, ingressOperationFailure{cause: errors.Join(ErrInvalidConfig, err)}
	}
	if base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || auth == nil {
		return nil, ingressOperationFailure{cause: ErrInvalidConfig}
	}
	endpoint := *base
	endpoint.Path = strings.TrimRight(base.Path, "/") + "/v1/browser-access/ingress/authorize"
	endpoint.RawPath = ""
	client := &http.Client{Transport: errorreport.TransportOperation(transport, base.String(), "browser_authorization"), Timeout: connectorprotocol.IngressAuthorityLifetime, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalidConfig }}
	return func(ctx context.Context, open connectorprotocol.StreamOpen, decision connectorprotocol.IngressDecision) (current connectorprotocol.IngressDecision, resultErr error) {
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
			return denied, ingressOperationFailure{cause: err}
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		operation := "browser-ingress-" + hex.EncodeToString(nonce[:])
		token, err := auth.Token(ctx)
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if strings.TrimSpace(token) == "" {
			return denied, ingressOperationFailure{cause: ErrProductionCredentialMissing}
		}
		proof, err := auth.Proof(ctx, operation, http.MethodPost, endpoint.Path, body)
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if len(proof) == 0 {
			return denied, ingressOperationFailure{cause: ErrProductionCredentialMissing}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		request.Header.Set("X-Paperboat-Machine-Identity", token)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
		request.Header.Set("Idempotency-Key", operation)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		defer func() {
			if closeErr := response.Body.Close(); closeErr != nil {
				current = denied
				resultErr = ingressOperationFailure{cause: errors.Join(resultErr, closeErr)}
			}
		}()
		if response.StatusCode != http.StatusOK {
			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
				return denied, connectorprotocol.ErrIngressDenied
			}
			return denied, ingressOperationFailure{cause: errorreport.HTTPStatusFailure(response)}
		}
		contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if contentType != "application/json" {
			return denied, ingressOperationFailure{cause: ErrProductionAssemblyInvalid}
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if len(raw) > 64<<10 {
			return denied, ingressOperationFailure{cause: ErrProductionAssemblyInvalid}
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		start, err := decoder.Token()
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if start != json.Delim('{') {
			return denied, ingressOperationFailure{cause: ErrProductionAssemblyInvalid}
		}
		key, err := decoder.Token()
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if key != "data" {
			return denied, ingressOperationFailure{cause: ErrProductionAssemblyInvalid}
		}
		var payload json.RawMessage
		if err := decoder.Decode(&payload); err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		end, err := decoder.Token()
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}
		if end != json.Delim('}') {
			return denied, ingressOperationFailure{cause: ErrProductionAssemblyInvalid}
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return denied, ingressOperationFailure{cause: errors.Join(ErrProductionAssemblyInvalid, err)}
		}
		// Reuse the strict connector decoder, including duplicate-key rejection.
		var wire bytes.Buffer
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
		wire.Write(length[:])
		wire.Write(payload)
		current, err = connectorprotocol.ReadIngressDecision(&wire, time.Now().UTC())
		if err != nil {
			return denied, ingressOperationFailure{cause: err}
		}

		expected := decision
		expected.IssuedAt, expected.ExpiresAt = current.IssuedAt, current.ExpiresAt
		if expected.Authorize(current, open, decision.EdgeNodeID, decision.EdgeProcessEpoch, time.Now().UTC()) != nil {
			return denied, connectorprotocol.ErrIngressDenied
		}
		return current, nil
	}, nil
}

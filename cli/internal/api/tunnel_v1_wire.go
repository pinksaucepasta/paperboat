package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func redactTunnelMetadata(value map[string]any, depth int) (map[string]any, error) {
	if depth > 6 || len(value) > 64 {
		return nil, ErrUnsafeTunnelResponse
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		if key == "" || len(key) > 128 || strings.ContainsAny(key, "\x00\r\n") {
			return nil, ErrUnsafeTunnelResponse
		}
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "private_key") {
			out[key] = "[REDACTED]"
			continue
		}
		switch typed := item.(type) {
		case map[string]any:
			nested, err := redactTunnelMetadata(typed, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = nested
		case []any:
			if len(typed) > 128 {
				return nil, ErrUnsafeTunnelResponse
			}
			copyValue := make([]any, len(typed))
			for i, v := range typed {
				if nested, ok := v.(map[string]any); ok {
					clean, err := redactTunnelMetadata(nested, depth+1)
					if err != nil {
						return nil, err
					}
					copyValue[i] = clean
				} else if text, ok := v.(string); ok {
					if len(text) > 1000 {
						return nil, ErrUnsafeTunnelResponse
					}
					copyValue[i] = redactTunnelText(text)
				} else if number, ok := v.(float64); ok {
					if math.IsNaN(number) || math.IsInf(number, 0) {
						return nil, ErrUnsafeTunnelResponse
					}
					copyValue[i] = number
				} else if v == nil {
					copyValue[i] = nil
				} else if _, ok := v.(bool); ok {
					copyValue[i] = v
				} else {
					return nil, ErrUnsafeTunnelResponse
				}
			}
			out[key] = copyValue
		case string:
			if len(typed) > 1000 {
				return nil, ErrUnsafeTunnelResponse
			}
			out[key] = redactTunnelText(typed)
		case float64:
			if math.IsNaN(typed) || math.IsInf(typed, 0) {
				return nil, ErrUnsafeTunnelResponse
			}
			out[key] = item
		case bool, nil:
			out[key] = item
		default:
			return nil, ErrUnsafeTunnelResponse
		}
	}
	return out, nil
}

func redactTunnelText(value string) string {
	if len(value) > 4096 {
		return "[REDACTED]"
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"bearer ", "token", "secret", "password", "private_key", "authorization:", "cookie"} {
		if strings.Contains(lower, marker) {
			return "[REDACTED]"
		}
	}
	return value
}

func responseETag(headers http.Header, body string) error {
	h := headers.Get("ETag")
	if h == "" || h != body {
		return ErrUnsafeTunnelResponse
	}
	return nil
}

// tunnelWireError mirrors the complete v1 error envelope. Keep this separate
// from APIError so server-owned diagnostic fields never accidentally become
// part of a normal success response type.
type tunnelWireError struct {
	Schema           string         `json:"schema"`
	Kind             string         `json:"kind"`
	Code             string         `json:"code"`
	Component        string         `json:"component"`
	Message          string         `json:"message"`
	Outcome          string         `json:"outcome"`
	Retryable        *bool          `json:"retryable"`
	RetryAt          *time.Time     `json:"retry_at"`
	RepairAction     string         `json:"repair_action"`
	RequestID        string         `json:"request_id"`
	CorrelationID    string         `json:"correlation_id"`
	SupportReference string         `json:"support_reference"`
	Details          map[string]any `json:"details"`
}

type tunnelWireEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error tunnelWireError `json:"error"`
}

// decodeTunnelJSONStrict rejects duplicate object keys as well as unknown or
// trailing fields. encoding/json alone rejects unknown fields but silently
// accepts duplicates, which is unsafe for signed/idempotent request and
// response documents.
func decodeTunnelJSONStrict(raw []byte, out any) error {
	if err := rejectTunnelDuplicateJSON(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func rejectTunnelDuplicateJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				key, ok := (func() (string, bool) {
					token, err := decoder.Token()
					if err != nil {
						return "", false
					}
					value, ok := token.(string)
					return value, ok
				})()
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("duplicate JSON field %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return fmt.Errorf("trailing JSON: %w", err)
	}
	return nil
}

func decodeTunnelData(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("empty response data")
	}
	return decodeTunnelJSONStrict(raw, out)
}

func (c *Client) doTunnelRequest(ctx context.Context, method, requestPath string, body any, out any, headers http.Header, responseHeaders *http.Header) error {
	if c == nil || strings.TrimSpace(c.baseURL) == "" {
		return errors.New("paperboat-server base URL is not configured")
	}
	if requestPath == "" || !strings.HasPrefix(requestPath, "/v1/") || strings.ContainsAny(requestPath, "\x00\r\n") || strings.Contains(requestPath, "://") {
		return errors.New("invalid tunnel API path")
	}
	var err error
	requestPath, err = c.workspaceRequestPath(requestPath)
	if err != nil {
		return err
	}
	var encodedBody []byte
	if body != nil {
		var err error
		encodedBody, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode tunnel request body: %w", err)
		}
		if len(encodedBody) > maxTunnelRequestBytes {
			return ErrUnsafeTunnelResponse
		}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(encodedBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paperboat/"+buildinfo.Version)
	req.Header.Set("X-Paperboat-Client", "paperboat")
	req.Header.Set("X-Paperboat-Protocol", buildinfo.ProtocolVersion)
	requestSupportReference := supportref.FromContext(ctx)
	if requestSupportReference == "" {
		requestSupportReference = supportref.New()
	}
	if requestSupportReference != "" {
		req.Header.Set(supportref.Header, requestSupportReference)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.authorizeUserRequest(req); err != nil {
		return err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if c.machineAuth != nil && machineAuthorizedRequest(method, req.URL.Path) {
		token, tokenErr := c.machineAuth.Token(ctx)
		if tokenErr != nil {
			return fmt.Errorf("machine authentication token: %w", tokenErr)
		}
		if strings.TrimSpace(token) == "" {
			return errors.New("machine authentication token is empty")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Paperboat-Machine-Identity", token)
		operationID := strings.TrimSpace(req.Header.Get("Idempotency-Key"))
		if operationID == "" {
			return errors.New("machine-authenticated mutation requires Idempotency-Key")
		}
		// Match the canonical route path used by the server verifier; the
		// signed body carries workspace selection independently of the query.
		proof, proofErr := c.machineAuth.Proof(ctx, operationID, method, req.URL.Path, encodedBody)
		if proofErr != nil {
			return fmt.Errorf("machine authentication proof: %w", proofErr)
		}
		if len(proof) == 0 {
			return errors.New("machine authentication proof is empty")
		}
		req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	}
	httpClient := c.http
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	// Clone the configured client so this request cannot inherit the default
	// redirect-following behavior, and never send auth headers to a Location.
	redirectSafeClient := *httpClient
	redirectSafeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return ErrTunnelRedirect }
	resp, err := redirectSafeClient.Do(req)
	if err != nil {
		if errors.Is(err, ErrTunnelRedirect) {
			return ErrTunnelRedirect
		}
		return fmt.Errorf("call %s %s: %w", method, requestPath, err)
	}
	defer resp.Body.Close()
	if responseHeaders != nil {
		*responseHeaders = resp.Header.Clone()
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTunnelResponseBytes+1))
	if readErr != nil {
		return fmt.Errorf("read %s %s response: %w", method, requestPath, readErr)
	}
	if len(raw) > maxTunnelResponseBytes {
		return ErrUnsafeTunnelResponse
	}
	if resp.StatusCode == http.StatusNoContent {
		if out != nil && len(bytes.TrimSpace(raw)) != 0 {
			return ErrUnsafeTunnelResponse
		}
		return nil
	}
	var envelope tunnelWireEnvelope
	if err := decodeTunnelJSONStrict(raw, &envelope); err != nil {
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &APIError{Status: resp.StatusCode, Code: "invalid_server_response", Message: "paperboat-server returned an invalid error response", RequestID: responseRequestID(resp.Header), SupportReference: responseOrRequestSupportReference(resp.Header, "", requestSupportReference), cause: errors.Join(errorreport.HTTPStatusFailure(resp), &ResponseDecodeError{Err: err})}
		}
		return &ResponseDecodeError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUpgradeRequired || envelope.Error.Code == "incompatible_client_version" {
			required, _ := envelope.Error.Details["required_protocol"].(string)
			return &ErrIncompatibleVersion{Required: safeRequiredProtocol(required), Message: apiErrorMessage("incompatible_client_version", resp.StatusCode)}
		}
		details, detailsErr := redactTunnelMetadata(envelope.Error.Details, 0)
		if detailsErr != nil {
			details = nil
		}
		code := envelope.Error.Code
		if code == "" {
			code = "server_error"
		}
		return c.workspaceRequestError(requestPath, &APIError{Status: resp.StatusCode, Code: code, Message: apiErrorMessage(code, resp.StatusCode), RequestID: responseRequestID(resp.Header), SupportReference: responseOrRequestSupportReference(resp.Header, envelope.Error.SupportReference, requestSupportReference), Details: details, cause: errorreport.HTTPStatusFailure(resp)})
	}
	if out == nil {
		return nil
	}
	if err := decodeTunnelData(envelope.Data, out); err != nil {
		return &ResponseDecodeError{Err: err}
	}
	return nil
}

func responseOrRequestSupportReference(header http.Header, bodyReference, requestReference string) string {
	if value := header.Get(supportref.Header); supportref.Valid(value) {
		return value
	}
	if supportref.Valid(bodyReference) {
		return bodyReference
	}
	if supportref.Valid(requestReference) {
		return requestReference
	}
	return ""
}

func tunnelResponseKind(raw json.RawMessage) (string, error) {
	var discriminator struct {
		Kind string `json:"kind"`
	}
	if err := rejectTunnelDuplicateJSON(raw); err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return "", err
	}
	return discriminator.Kind, nil
}

// UnmarshalJSON accepts the canonical resource or operation returned by the server.
// It retains the existing Go mutation result shape used by
// pb while accepting that wire union and the older test/integration wrapper.
func (m *TunnelMutation) UnmarshalJSON(raw []byte) error {
	*m = TunnelMutation{}
	kind, err := tunnelResponseKind(raw)
	if err != nil {
		return err
	}
	switch kind {
	case "tunnel":
		if err := decodeTunnelJSONStrict(raw, &m.Tunnel); err != nil {
			return err
		}
	case "operation":
		if err := decodeTunnelJSONStrict(raw, &m.Operation); err != nil {
			return err
		}
		m.Tunnel.ID = m.Operation.ResourceID
	default:
		type plain TunnelMutation
		var value plain
		if err := decodeTunnelJSONStrict(raw, &value); err != nil {
			return err
		}
		*m = TunnelMutation(value)
	}
	return nil
}

func (m *TunnelRouteMutation) UnmarshalJSON(raw []byte) error {
	*m = TunnelRouteMutation{}
	kind, err := tunnelResponseKind(raw)
	if err != nil {
		return err
	}
	switch kind {
	case "route":
		if err := decodeTunnelJSONStrict(raw, &m.Route); err != nil {
			return err
		}
	case "operation":
		if err := decodeTunnelJSONStrict(raw, &m.Operation); err != nil {
			return err
		}
		m.Route.ID = m.Operation.ResourceID
	default:
		type plain TunnelRouteMutation
		var value plain
		if err := decodeTunnelJSONStrict(raw, &value); err != nil {
			return err
		}
		*m = TunnelRouteMutation(value)
	}
	return nil
}

func (m *TunnelDomainMutation) UnmarshalJSON(raw []byte) error {
	*m = TunnelDomainMutation{}
	kind, err := tunnelResponseKind(raw)
	if err != nil {
		return err
	}
	switch kind {
	case "domain_binding":
		if err := decodeTunnelJSONStrict(raw, &m.Domain); err != nil {
			return err
		}
	case "operation":
		if err := decodeTunnelJSONStrict(raw, &m.Operation); err != nil {
			return err
		}
		m.Domain.ID = m.Operation.ResourceID
	default:
		type plain TunnelDomainMutation
		var value plain
		if err := decodeTunnelJSONStrict(raw, &value); err != nil {
			return err
		}
		*m = TunnelDomainMutation(value)
	}
	return nil
}

func (m *TunnelConnectorMutation) UnmarshalJSON(raw []byte) error {
	*m = TunnelConnectorMutation{}
	kind, err := tunnelResponseKind(raw)
	if err != nil {
		return err
	}
	switch kind {
	case "connector":
		if err := decodeTunnelJSONStrict(raw, &m.Connector); err != nil {
			return err
		}
	case "operation":
		if err := decodeTunnelJSONStrict(raw, &m.Operation); err != nil {
			return err
		}
		m.Connector.ID = m.Operation.ResourceID
	default:
		type plain TunnelConnectorMutation
		var value plain
		if err := decodeTunnelJSONStrict(raw, &value); err != nil {
			return err
		}
		*m = TunnelConnectorMutation(value)
	}
	return nil
}

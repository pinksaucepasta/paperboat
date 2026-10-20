// Package api is the Paperboat bearer-authenticated control-plane client.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// Client talks to paperboat-server with a Paperboat client-session access token.
type Client struct {
	baseURL          string
	cred             config.Credential
	http             *http.Client
	accessToken      string
	credentialSource config.AuthSource
	sourceMachineID  string
	machineAuth      MachineAuthSource
	workspace        string
}

// SetCredentialSource keeps user API requests bound to renewable account credentials.
// The source owns refresh, persistence and lifetime; machine-proof routes remain separate.
func (c *Client) SetCredentialSource(source config.AuthSource) { c.credentialSource = source }

func (c *Client) authorizeUserRequest(req *http.Request) error {
	if c.machineAuth != nil && machineAuthorizedRequest(req.Method, req.URL.Path) {
		return nil
	}
	token := c.accessToken
	if c.credentialSource != nil {
		credential, err := c.credentialSource.Credential()
		if err != nil {
			return fmt.Errorf("account authentication credential: %w", err)
		}
		token = strings.TrimSpace(credential.AccessToken)
		if token == "" {
			return ErrUnauthenticated
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// MachineAuthSource supplies the renewable machine-control bearer and signs
// the exact request body for a mutating operation. It is intentionally a
// narrow interface so the API client never persists or owns machine secrets.
type MachineAuthSource interface {
	Token(context.Context) (string, error)
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

// SetMachineAuth authenticates the explicit host-owned API endpoints with
// machine identity. Other endpoints retain client-session authentication. Host
// mutations must include Idempotency-Key; that key is bound into the signed proof. GETs retain ordinary client-session authentication so
// a create operation can fetch its public lease through the read middleware.
// The source remains responsible for renewal and key material. Passing nil
// restores ordinary client-session authentication for mutations.
func (c *Client) SetMachineAuth(source MachineAuthSource) {
	if c == nil {
		return
	}
	c.machineAuth = source
}

// machineAuthorizedRequest mirrors the server's machine-proof routes. Merely
// configuring a host signer must never replace a user credential on resource
// management APIs (including domains, routes, and credential rotation).
func machineAuthorizedRequest(method, path string) bool {
	if method == http.MethodPost {
		switch path {
		case "/v1/previews", "/v1/tunnels", "/v1/usage/native", "/v1/machine-peer-network/register", "/v1/machine-peer-network/config":
			return true
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[2] == "" {
		return false
	}
	switch parts[1] {
	case "previews":
		return method == http.MethodDelete && len(parts) == 3 || method == http.MethodPost && (len(parts) == 4 && parts[3] == "readiness" || len(parts) == 5 && parts[3] == "lease" && parts[4] == "renew")
	case "tunnels":
		return method == http.MethodPost && (len(parts) == 5 && parts[3] == "connectors" && parts[4] == "enrollments" || len(parts) == 6 && parts[3] == "connectors" && parts[4] == "enrollments" && parts[5] == "exchange")
	}
	return false
}

func (c *Client) SetSourceMachineID(machineID string) {
	c.sourceMachineID = strings.TrimSpace(machineID)
}

// New builds a client. baseURL is the paperboat-server base (e.g.
// https://api.paperboat.dev). httpClient is optional; a sane default with a
// timeout is used when nil.
func New(baseURL string, cred config.Credential, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}
	clientCopy := *httpClient
	clientCopy.Transport = errorreport.Transport(httpClient.Transport, baseURL)
	httpClient = &clientCopy
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		cred:        cred,
		http:        httpClient,
		accessToken: strings.TrimSpace(cred.AccessToken),
	}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doRequest(ctx, method, path, body, out, nil, false)
}

func (c *Client) doWithHeaders(ctx context.Context, method, path string, body, out any, headers http.Header) error {
	return c.doRequest(ctx, method, path, body, out, headers, false)
}

func (c *Client) doStrict(ctx context.Context, method, path string, body, out any) error {
	return c.doRequest(ctx, method, path, body, out, nil, true)
}

func (c *Client) doRequest(ctx context.Context, method, path string, body, out any, headers http.Header, strict bool) error {
	return c.doRequestMeta(ctx, method, path, body, out, headers, strict, nil)
}

// newRequest builds one authenticated request of the shared client: workspace routing, client
// identification headers, a support reference and the user's credential.
func (c *Client) newRequest(ctx context.Context, method, path string, body any, headers http.Header) (req *http.Request, encodedBody []byte, requestSupportReference, resolvedPath string, err error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return nil, nil, "", path, errors.New("paperboat-server base URL is not configured")
	}
	path, err = c.workspaceRequestPath(path)
	if err != nil {
		return nil, nil, "", path, err
	}

	var reader io.Reader
	if body != nil {
		encoded, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, nil, "", path, fmt.Errorf("encode request body: %w", marshalErr)
		}
		encodedBody = encoded
		reader = bytes.NewReader(encoded)
	}

	req, err = http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, "", path, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paperboat/"+buildinfo.Version)
	req.Header.Set("X-Paperboat-Client", "paperboat")
	req.Header.Set("X-Paperboat-Distribution", buildinfo.Distribution)
	req.Header.Set("X-Paperboat-Protocol", buildinfo.ProtocolVersion)
	requestSupportReference = supportref.FromContext(ctx)
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
		return nil, nil, "", path, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return req, encodedBody, requestSupportReference, path, nil
}

// doRequestMeta is the same authenticated request path used by the rest of
// the client, with response headers exposed to callers that need optimistic
// concurrency metadata. Keeping this as a narrow extension avoids making
// ETags or other transport headers part of every existing API method.
func (c *Client) doRequestMeta(ctx context.Context, method, path string, body, out any, headers http.Header, strict bool, responseHeaders *http.Header) error {
	req, encodedBody, requestSupportReference, path, err := c.newRequest(ctx, method, path, body, headers)
	if err != nil {
		return err
	}
	if c.machineAuth != nil && machineAuthorizedRequest(method, req.URL.Path) {
		token, err := c.machineAuth.Token(ctx)
		if err != nil {
			return fmt.Errorf("machine authentication token: %w", err)
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
		// Machine proofs use the canonical route path verified by the server.
		// Workspace selection remains authenticated in the exact signed JSON body.
		proof, err := c.machineAuth.Proof(ctx, operationID, method, req.URL.Path, encodedBody)
		if err != nil || len(proof) == 0 {
			if err == nil {
				err = errors.New("empty machine proof")
			}
			return fmt.Errorf("machine authentication proof: %w", err)
		}
		req.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if responseHeaders != nil {
		*responseHeaders = resp.Header.Clone()
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	var envelope apiEnvelope
	// A body is expected for every documented response; a decode failure on a
	// 2xx is a real protocol error, so surface it rather than silently succeed.
	decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.responseError(resp, path, requestSupportReference, envelope, decodeErr)
	}
	if decodeErr != nil {
		return &ResponseDecodeError{Err: decodeErr}
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("%s %s returned an empty response", method, path)
	}
	if !strict {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return &ResponseDecodeError{Err: err}
		}
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return &ResponseDecodeError{Err: err}
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s %s data: trailing JSON", method, path)
	}
	return nil
}

// apiEnvelope is the shared response body: data on success, error otherwise.
type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code             string         `json:"code"`
		Message          string         `json:"message"`
		SupportReference string         `json:"support_reference"`
		Details          map[string]any `json:"details"`
	} `json:"error"`
}

// responseError turns a non-2xx response into the client's typed error.
func (c *Client) responseError(resp *http.Response, path, requestSupportReference string, envelope apiEnvelope, decodeErr error) error {
	if (resp.StatusCode == http.StatusUpgradeRequired && envelope.Error.Code != "update_required") || envelope.Error.Code == "incompatible_client_version" {
		required, _ := envelope.Error.Details["required_protocol"].(string)
		return &ErrIncompatibleVersion{Required: safeRequiredProtocol(required), Message: apiErrorMessage("incompatible_client_version", resp.StatusCode)}
	}
	responseSupportReference := resp.Header.Get(supportref.Header)
	if !supportref.Valid(responseSupportReference) {
		responseSupportReference = envelope.Error.SupportReference
	}
	if !supportref.Valid(responseSupportReference) {
		responseSupportReference = requestSupportReference
	}
	cause := errorreport.HTTPStatusFailure(resp)
	if decodeErr != nil {
		cause = errors.Join(cause, &ResponseDecodeError{Err: decodeErr})
	}
	return c.workspaceRequestError(path, &APIError{Status: resp.StatusCode, Code: envelope.Error.Code, Message: apiErrorMessageFor(envelope.Error.Code, resp.StatusCode, envelope.Error.Details), RequestID: responseRequestID(resp.Header), SupportReference: responseSupportReference, Details: envelope.Error.Details, cause: cause})
}

func safeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 {
		return ""
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("_.:-", r) {
			return ""
		}
	}
	return value
}

func validManagedSSHHostKeySetID(value string) bool {
	suffix, ok := strings.CutPrefix(value, "keyset_")
	id, err := uuid.Parse(suffix)
	return ok && err == nil && id.Version() == 4 && id.Variant() == uuid.RFC4122 && id.String() == suffix
}

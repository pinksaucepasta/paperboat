package api

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DiagnosticUploadIntentRequestSchemaV1 = "paperboat.diagnostic-upload-intent-request/v1"
const DiagnosticUploadIntentSchemaV1 = "paperboat.diagnostic-upload-intent/v1"

type DiagnosticUploadIntentRequest struct {
	Schema        string   `json:"schema"`
	CorrelationID string   `json:"correlation_id"`
	Bytes         int64    `json:"bytes"`
	SHA256        string   `json:"sha256"`
	Categories    []string `json:"categories"`
}

type DiagnosticUploadIntent struct {
	Schema        string            `json:"schema"`
	IntentID      string            `json:"intent_id"`
	CorrelationID string            `json:"correlation_id"`
	State         string            `json:"state"`
	ExpiresAt     time.Time         `json:"expires_at"`
	UploadMethod  string            `json:"upload_method,omitempty"`
	UploadURL     string            `json:"upload_url,omitempty"`
	UploadHeaders map[string]string `json:"upload_headers,omitempty"`
}

func (c *Client) CreateDiagnosticUploadIntent(ctx context.Context, operationKey string, request DiagnosticUploadIntentRequest) (DiagnosticUploadIntent, error) {
	var result DiagnosticUploadIntent
	if request.Schema != DiagnosticUploadIntentRequestSchemaV1 || strings.TrimSpace(operationKey) == "" {
		return result, errors.New("invalid diagnostic upload intent request")
	}
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/diagnostic-upload-intents", request, &result, http.Header{"Idempotency-Key": []string{operationKey}})
	if err != nil {
		return DiagnosticUploadIntent{}, err
	}
	if err := result.validate(true); err != nil {
		return DiagnosticUploadIntent{}, err
	}
	return result, nil
}

func (c *Client) CompleteDiagnosticUploadIntent(ctx context.Context, intentID string) (DiagnosticUploadIntent, error) {
	var result DiagnosticUploadIntent
	if strings.TrimSpace(intentID) == "" {
		return result, errors.New("invalid diagnostic upload intent")
	}
	err := c.doChecked(ctx, http.MethodPost, "/v1/diagnostic-upload-intents/"+url.PathEscape(intentID)+"/complete", struct{}{}, &result)
	if err != nil {
		return DiagnosticUploadIntent{}, err
	}
	if err := result.validate(false); err != nil || result.State != "uploaded" {
		return DiagnosticUploadIntent{}, errors.New("paperboat-server returned an invalid diagnostic upload completion")
	}
	return result, nil
}

// UploadDiagnosticBundle borrows content until it returns and never closes it.
// Reads must finish promptly; the production caller supplies a regular bundle file.
func (c *Client) UploadDiagnosticBundle(ctx context.Context, intent DiagnosticUploadIntent, content io.Reader, bytes int64) error {
	if err := intent.validate(true); err != nil {
		return err
	}
	if intent.State != "pending" || content == nil || bytes <= 0 {
		return errors.New("invalid diagnostic upload")
	}
	body := &diagnosticUploadBody{reader: content}
	defer body.Close() // Fence asynchronous transport reads before releasing the borrowed reader.
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, intent.UploadURL, body)
	if err != nil {
		return &diagnosticUploadFailure{cause: err}
	}
	request.ContentLength = bytes
	for name, value := range intent.UploadHeaders {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canonical == "" || strings.ContainsAny(value, "\r\n") || forbiddenUploadHeader(canonical) {
			return errors.New("paperboat-server returned an unsafe diagnostic upload header")
		}
		if canonical == "Content-Length" {
			continue
		}
		request.Header.Set(canonical, value)
	}
	client := *c.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return &diagnosticUploadFailure{cause: err}
	}
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	var statusErr error
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		statusErr = errorreport.HTTPStatusFailure(response)
		if statusErr == nil {
			statusErr = errors.New("object storage rejected diagnostic bundle")
		}
	}
	if cause := errors.Join(statusErr, drainErr, closeErr); cause != nil {
		return &diagnosticUploadFailure{cause: cause}
	}
	return nil
}

func (value DiagnosticUploadIntent) validate(requireUpload bool) error {
	if value.Schema != DiagnosticUploadIntentSchemaV1 || !validDiagnosticID(value.IntentID) || !supportref.Valid(value.CorrelationID) || value.ExpiresAt.IsZero() || value.ExpiresAt.Location() != time.UTC || value.State != "pending" && value.State != "uploaded" {
		return errors.New("paperboat-server returned an invalid diagnostic upload intent")
	}
	if requireUpload && value.State == "pending" {
		u, err := url.Parse(value.UploadURL)
		if err != nil {
			return &diagnosticUploadAuthorityFailure{cause: err}
		}
		if u.Scheme != "https" || u.Host == "" || u.User != nil || value.UploadMethod != http.MethodPut || len(value.UploadHeaders) == 0 || len(value.UploadHeaders) > 8 {
			return errors.New("paperboat-server returned invalid diagnostic upload authority")
		}
	}
	if value.State == "uploaded" && (value.UploadMethod != "" || value.UploadURL != "" || len(value.UploadHeaders) != 0) {
		return errors.New("paperboat-server returned upload authority for a completed diagnostic upload")
	}
	return nil
}

func forbiddenUploadHeader(name string) bool {
	switch name {
	case "Authorization", "Cookie", "Proxy-Authorization", "Proxy-Authenticate", "Set-Cookie", "Host", "Connection", "Transfer-Encoding", "Trailer", "Upgrade":
		return true
	default:
		return strings.HasPrefix(name, "X-Paperboat-")
	}
}

func validDiagnosticID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// diagnosticUploadBody owns only transport access to the reader. Close fences
// active finite reads and prevents late reads; custody stays with the caller.
type diagnosticUploadBody struct {
	mu     sync.Mutex
	reader io.Reader
	closed bool
}

func (b *diagnosticUploadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	return b.reader.Read(p)
}
func (b *diagnosticUploadBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

type diagnosticUploadFailure struct{ cause error }

func (*diagnosticUploadFailure) Error() string {
	return "diagnostic bundle upload failed; retry pb bugreport --upload"
}
func (e *diagnosticUploadFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Keep malformed authority parser causes inspectable without displaying a
// presigned URL, its query, or any other server-provided text.
type diagnosticUploadAuthorityFailure struct{ cause error }

func (*diagnosticUploadAuthorityFailure) Error() string {
	return "paperboat-server returned invalid diagnostic upload authority"
}
func (e *diagnosticUploadAuthorityFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

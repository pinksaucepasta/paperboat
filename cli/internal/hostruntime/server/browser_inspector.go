package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/admission"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
)

const (
	browserInspectorBodyBytes     = 64 << 10
	browserInspectorResponseBytes = (2 << 20) + (100 << 10)
	browserInspectorQueryFields   = 16
	browserInspectorTimeout       = 15 * time.Second
)

// BrowserInspector serves the daemon inspector API for one principal. It is
// implemented by inspectorapi.Service.
type BrowserInspector interface {
	ServeForPrincipal(principal string, writer http.ResponseWriter, request *http.Request) (mismatch bool)
}

type browserInspectorRequest struct {
	Grant  string            `json:"grant"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  map[string]string `json:"query,omitempty"`
	Body   json.RawMessage   `json:"body,omitempty"`
}

type browserInspectorResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// browserInspector serves one inspector API call over the console's E2EE
// inspector channel. The grant is re-verified live by the inspector service
// and must belong to the user the channel credential names.
func (d *Dispatcher) browserInspector(ctx context.Context, a Authorization, payload json.RawMessage) operation.Outcome {
	if d.config.Inspector == nil {
		return failure("capability_disabled")
	}
	if !a.BrowserInspector || a.BrowserTerminal || a.UserID == "" || a.AccountID != a.UserID || a.BrowserAttachmentID == "" || a.ClientID != a.BrowserAttachmentID {
		return failure("not_found_or_forbidden")
	}
	var request browserInspectorRequest
	if len(payload) > browserInspectorBodyBytes+4<<10 || decodeStrict(payload, &request) != nil || !validBrowserInspectorRequest(request) {
		return failure("invalid_request")
	}
	lease, err := admission.Admit(ctx, time.Time{}, a.RevokedSignal)
	if err != nil {
		if errors.Is(err, admission.ErrRevoked) {
			return failure("operation_canceled")
		}
		return domainResult(nil, err)
	}
	defer lease.Close()
	requestCtx, cancel := context.WithTimeout(lease.Context(), browserInspectorTimeout)
	defer cancel()
	target := &url.URL{Path: request.Path}
	if len(request.Query) > 0 {
		values := url.Values{}
		for key, value := range request.Query {
			values.Set(key, value)
		}
		target.RawQuery = values.Encode()
	}
	httpRequest, err := http.NewRequestWithContext(requestCtx, request.Method, target.String(), bytes.NewReader(request.Body))
	if err != nil {
		return failure("invalid_request")
	}
	httpRequest.Header.Set("X-Paperboat-Inspector-Grant", request.Grant)
	if len(request.Body) > 0 {
		httpRequest.Header.Set("Content-Type", "application/json")
	}
	writer := &boundedResponseWriter{header: http.Header{}}
	if d.config.Inspector.ServeForPrincipal(a.UserID, writer, httpRequest) {
		return failure("not_found_or_forbidden")
	}
	if writer.overflow {
		return failure("response_too_large")
	}
	if err := requestCtx.Err(); err != nil {
		return domainResult(nil, err)
	}
	body := bytes.TrimSpace(writer.body.Bytes())
	if len(body) == 0 {
		body = []byte("null")
	}
	if !json.Valid(body) {
		return failure("invalid_response")
	}
	return result(browserInspectorResult{Status: writer.statusCode(), Body: body})
}

func validBrowserInspectorRequest(r browserInspectorRequest) bool {
	if r.Grant == "" || len(r.Grant) > 512 || len(r.Query) > browserInspectorQueryFields {
		return false
	}
	for key, value := range r.Query {
		if key == "" || len(key) > 64 || len(value) > 512 {
			return false
		}
	}
	body := len(bytes.TrimSpace(r.Body)) > 0 && string(bytes.TrimSpace(r.Body)) != "null"
	if body && (len(r.Body) > browserInspectorBodyBytes || r.Method != http.MethodPut && r.Method != http.MethodPost) {
		return false
	}
	switch r.Path {
	case "/v1/inspector/records":
		return r.Method == http.MethodGet || r.Method == http.MethodDelete
	case "/v1/inspector/policy":
		return r.Method == http.MethodPut && body
	case "/v1/inspector/replay":
		return r.Method == http.MethodPost && body
	case "/v1/inspector/audit":
		return r.Method == http.MethodGet
	}
	id, ok := strings.CutPrefix(r.Path, "/v1/inspector/records/")
	return ok && r.Method == http.MethodGet && validInspectorRecordID(id)
}

func validInspectorRecordID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// boundedResponseWriter keeps one inspector response in memory up to
// browserInspectorResponseBytes; a larger response is refused, never cut.
type boundedResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	overflow bool
}

func (w *boundedResponseWriter) Header() http.Header { return w.header }

func (w *boundedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *boundedResponseWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.overflow || w.body.Len()+len(data) > browserInspectorResponseBytes {
		w.overflow = true
		return 0, errInspectorResponseTooLarge
	}
	return w.body.Write(data)
}

var errInspectorResponseTooLarge = errors.New("inspector response exceeds the browser channel limit")

func (w *boundedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

package control

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// RequestFailure carries a compile-time control path, status and fixed category. For an HTTP answer it also
// keeps who answered (the server's Request-Id, else a proxy's Via/Server header) and a short, character-filtered
// excerpt of the body, so a refusal names its responder; credentials and transport error text are excluded.
type RequestFailure struct {
	Path             string
	Status           int
	Category         string
	SupportReference string
	Responder        string
	Body             string
	Err              error
	Cause            error
}

const responseExcerptBytes = 200

// responseFailure reads a non-2xx answer (bounded) and keeps its responder and body excerpt.
func responseFailure(ctx context.Context, path string, response *http.Response, maximum int64) *RequestFailure {
	head := make([]byte, responseExcerptBytes)
	n, _ := io.ReadFull(response.Body, head)
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximum))
	responder := ""
	if id := response.Header.Get("Request-Id"); id != "" {
		responder = "server request " + id
	} else if via := response.Header.Get("Via"); via != "" {
		responder = "via " + via
	} else if server := response.Header.Get("Server"); server != "" {
		responder = "server header " + server
	}
	return &RequestFailure{Path: path, Status: response.StatusCode, Category: "http_status", SupportReference: controlReference(ctx),
		Responder: excerpt(responder), Body: excerpt(string(head[:n])), Err: ErrControlUnavailable}
}

// excerpt keeps printable ASCII only, so a log line cannot be forged or carry binary, and caps the length.
func excerpt(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return ' '
		}
		return r
	}, value)
	return strings.TrimSpace(value[:min(len(value), responseExcerptBytes)])
}

func (e *RequestFailure) Error() string   { return "control request failed" }
func (e *RequestFailure) Unwrap() []error { return []error{e.Err, e.Cause} }
func (e *RequestFailure) DiagnosticPath() string {
	if e == nil {
		return ""
	}
	return e.Path
}
func (e *RequestFailure) DiagnosticResponse() (responder, body string) {
	if e == nil {
		return "", ""
	}
	return e.Responder, e.Body
}
func (e *RequestFailure) DiagnosticStatus() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// documentFailure retains a decoder/parser cause without retaining private
// input in the public message. Unlike RequestFailure, its semantic validation
// has not already been published by the HTTP attempt owner.
type documentFailure struct {
	sentinel error
	cause    error
}

func (e documentFailure) Error() string   { return "control document is invalid" }
func (e documentFailure) Unwrap() []error { return []error{e.sentinel, e.cause} }

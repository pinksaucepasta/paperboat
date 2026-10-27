package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// ErrUnauthenticated means the server rejected the reused credential. Callers
// should route the user through Paperboat machine login.
var ErrUnauthenticated = errors.New("paperboat-server rejected the credential")

// ErrMachineAuthReadRequiresClientSession reports that a machine-proof create
// returned an operation but this client has no ordinary read credential to
// fetch the resulting public lease. The server deliberately keeps preview
// reads behind the client-session authorization boundary.
var ErrMachineAuthReadRequiresClientSession = errors.New("machine-authenticated preview create requires a client-session read credential")

// ErrIncompatibleVersion tells callers to upgrade instead of retrying.
type ErrIncompatibleVersion struct{ Required, Message string }

func (e *ErrIncompatibleVersion) Error() string {
	if required := safeRequiredProtocol(e.Required); required != "" {
		return fmt.Sprintf("this CLI is incompatible with the server (required protocol %s); upgrade pb", required)
	}
	return "this CLI is incompatible with the server; upgrade pb"
}

func responseRequestID(header http.Header) string {
	if requestID := safeRequestID(header.Get("Request-Id")); requestID != "" {
		return requestID
	}
	return safeRequestID(header.Get("X-Request-ID"))
}

// APIError is a structured server error surfaced to the caller. It carries the
// server's stable error code so command logic can branch without string
// matching on messages.
type APIError struct {
	Status  int
	Code    string
	Message string
	// ServerMessage is the message the server itself sent (bounded, control characters removed). Message is
	// the client's authored text for the code; a 4xx shows the server's own words, see Refusal.
	ServerMessage    string
	RequestID        string
	SupportReference string
	Details          map[string]any
	// RetryAfter is the response's Retry-After in whole seconds, 0 when absent.
	RetryAfter time.Duration
	// Team is set by the CLI command that knows which team the request was about; it is never decoded
	// from the response. A team_suspended refusal names it.
	Team  string
	cause error
}

func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// DiagnosticStatus exposes only the protocol status to safe fault projection.
// The response message, details and request data remain outside logging sinks.
func (e *APIError) DiagnosticStatus() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// IsNotFound reports whether the control plane explicitly rejected a request
// because its resource or route is absent. Callers use it only for additive
// capability discovery; authorization failures are never treated as absent.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

func (e *APIError) Error() string {
	if e == nil {
		return "paperboat-server request failed"
	}
	if e.Code == "team_suspended" && e.Team != "" {
		return fmt.Sprintf("Team %q is suspended, so it cannot be changed. Ask a team owner to restore billing in the console; access returns when the team resumes.", e.Team)
	}
	return apiErrorMessageFor(e.Code, e.Status, e.Details)
}

func (e *APIError) Is(target error) bool {
	return e != nil && e.Status == http.StatusUnauthorized && target == ErrUnauthenticated
}

// Retryable classifies transport-level HTTP failures for lease/session
// renewals without making callers branch on human-readable error messages.
func (e *APIError) Retryable() bool {
	return e != nil && (e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests || e.Status >= 500)
}

// Unreachable reports that Paperboat itself could not be reached, the only condition for starting
// a session from the host's own signed scopes (PLAN 3b "Offline-capable authority" point 3): the
// request did not complete (connection, TLS or timeout) or the API origin answered 503. Any other
// answer, a refusal included, is final.
func Unreachable(err error) bool {
	var request *url.Error
	if errors.As(err, &request) {
		return true
	}
	var response *APIError
	return errors.As(err, &response) && response.Status == http.StatusServiceUnavailable
}

// BoundedServerMessage is a server error message safe to print on one line: trimmed, control characters
// dropped, at most 256 characters.
func BoundedServerMessage(message string) string {
	var out []rune
	for _, r := range strings.TrimSpace(message) {
		if unicode.IsControl(r) {
			continue
		}
		if out = append(out, r); len(out) == 256 {
			break
		}
	}
	return string(out)
}

var serverCodeShape = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Refusal is what the server said about a request the CLI has no text for: its own code and message, so a refusal
// is never hidden behind a generic failure. It applies to the statuses that carry a refusal (400, 403, 404, 409,
// 422, 429), never to a code the CLI already words itself (publicAPIMessages) and never without a server message.
// The code is returned only when it has the shape of a server constant.
func (e *APIError) Refusal() (code, message string, ok bool) {
	if e == nil || e.ServerMessage == "" {
		return "", "", false
	}
	switch e.Status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
	default:
		return "", "", false
	}
	if _, mapped := publicAPIMessages[e.Code]; mapped {
		return "", "", false
	}
	if serverCodeShape.MatchString(e.Code) {
		code = e.Code
	}
	return code, e.ServerMessage, true
}

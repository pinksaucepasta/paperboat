// Package clientapi owns host-independent control-plane request mechanics.
package clientapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"io"
	"net/http"
	"strings"
)

const MaxResponseBytes = 2 << 20

var ErrResponseTooLarge = errors.New("paperboat-server response exceeds the size limit")

type CallFailure struct {
	Message string
	Err     error
}

func (e *CallFailure) Error() string { return e.Message }
func (e *CallFailure) Unwrap() error { return e.Err }

type Envelope struct {
	Data  json.RawMessage `json:"data"`
	Error struct {
		Code             string         `json:"code"`
		Message          string         `json:"message"`
		Details          map[string]any `json:"details"`
		SupportReference string         `json:"support_reference"`
	} `json:"error"`
}

// CallOptions keeps product identity, typed error projection and observation in the caller.
type CallOptions struct {
	HTTPClient      *http.Client
	Headers         http.Header
	DecodeError     func(error) error
	ResponseError   func(*http.Response, Envelope, string, error) error
	StatusFailure   func(*http.Response) error
	AttemptObserved func(error) bool
	ObserveFailure  func(context.Context, error) error
}
type ResponseError struct {
	Status int
	Code   string
	Err    error
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("paperboat-server refused request (HTTP %d)", e.Status)
}
func (e *ResponseError) Unwrap() error { return e.Err }
func (o CallOptions) decodeError(err error) error {
	if o.DecodeError != nil {
		return o.DecodeError(err)
	}
	return &CallFailure{Message: "paperboat-server response could not be decoded", Err: err}
}
func (o CallOptions) responseError(resp *http.Response, env Envelope, ref string, cause error) error {
	if o.ResponseError != nil {
		return o.ResponseError(resp, env, ref, cause)
	}
	return &ResponseError{Status: resp.StatusCode, Code: env.Error.Code, Err: cause}
}
func (o CallOptions) statusFailure(resp *http.Response) error {
	if o.StatusFailure != nil {
		return o.StatusFailure(resp)
	}
	return nil
}
func (o CallOptions) attemptObserved(err error) bool {
	return o.AttemptObserved != nil && o.AttemptObserved(err)
}
func Call(ctx context.Context, baseURL, method, path string, body any, bearer string, out any, options CallOptions) (callErr error) {
	var hasAttempt, attemptObserved, independentFailure bool
	observationContext := ctx
	defer func() {
		if callErr != nil && hasAttempt && (!attemptObserved || independentFailure) && options.ObserveFailure != nil {
			callErr = options.ObserveFailure(observationContext, callErr)
		}
	}()
	hc := options.HTTPClient
	if hc == nil {
		return errors.New("control-plane HTTP client is not configured")
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return &CallFailure{Message: "paperboat-server request could not be encoded", Err: err}
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return &CallFailure{Message: "paperboat-server request could not be prepared", Err: err}
	}
	requestSupportReference := supportref.FromContext(ctx)
	if requestSupportReference == "" {
		requestSupportReference = supportref.New()
	}
	if requestSupportReference != "" {
		req.Header.Set(supportref.Header, requestSupportReference)
	}
	observationContext = ctx
	if observationContext == nil {
		observationContext = context.Background()
	}
	observationContext = supportref.WithContext(observationContext, requestSupportReference)
	req.Header.Set("Accept", "application/json")
	for key, values := range options.Headers {
		req.Header[key] = append([]string(nil), values...)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	hasAttempt = true
	resp, err := hc.Do(req)
	if err != nil {
		attemptObserved = options.attemptObserved(err)
		return &CallFailure{Message: "paperboat-server request failed", Err: err}
	}
	statusFailure := error(nil)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusFailure = options.statusFailure(resp)
		attemptObserved = options.attemptObserved(statusFailure)
	}
	if resp.StatusCode == http.StatusNoContent && out == nil {
		if closeErr := resp.Body.Close(); closeErr != nil {
			independentFailure = true
			return &CallFailure{Message: "paperboat-server response could not be closed", Err: closeErr}
		}
		return nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	closeErr := resp.Body.Close()
	tooLarge := len(raw) > MaxResponseBytes
	var env Envelope
	var decodeErr error
	if readErr == nil && !tooLarge {
		decodeErr = json.NewDecoder(bytes.NewReader(raw)).Decode(&env)
	}
	var responseCause error
	if readErr != nil {
		responseCause = errors.Join(responseCause, options.decodeError(readErr))
	}
	if tooLarge {
		responseCause = errors.Join(responseCause, options.decodeError(ErrResponseTooLarge))
	}
	if decodeErr != nil {
		responseCause = errors.Join(responseCause, options.decodeError(decodeErr))
	}
	if closeErr != nil {
		responseCause = errors.Join(responseCause, &CallFailure{Message: "paperboat-server response could not be closed", Err: closeErr})
	}
	independentFailure = responseCause != nil
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return options.responseError(resp, env, requestSupportReference, errors.Join(statusFailure, responseCause))
	}
	if responseCause != nil {
		if readErr == nil && !tooLarge && decodeErr == nil && closeErr != nil {
			return &CallFailure{Message: "paperboat-server response could not be closed", Err: closeErr}
		}
		return options.decodeError(responseCause)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			independentFailure = true
			return options.decodeError(err)
		}
		return nil
	}
	return nil
}

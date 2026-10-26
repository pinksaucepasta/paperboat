package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

const ClientID = "paperboat"

var ClientScopes = []string{
	"account:read", "clients:revoke", "machines:read", "machines:connect", "session:refresh", "diagnostics:upload",
	"previews:read", "previews:write", "tunnels:read", "tunnels:write", "operations:read", "operations:write",
}

type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type TokenSet struct {
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token"`
	TokenType          string `json:"token_type"`
	ExpiresIn          int    `json:"expires_in"`
	Scope              string `json:"scope"`
	CLIClientSessionID string `json:"cli_client_session_id"`
	// Enrollment is the one-shot machine enrollment an approved login for a joining device carries.
	Enrollment *DeviceEnrollment `json:"enrollment,omitempty"`
}

// DeviceEnrollment is the machine enrollment minted for the workspace the account owner chose.
type DeviceEnrollment struct {
	BootstrapToken string    `json:"bootstrap_token"`
	ServerURL      string    `json:"server_url"`
	ExpiresAt      time.Time `json:"expires_at"`
	WorkspaceID    string    `json:"workspace_id"`
	// Admission is "pending" when the team must accept the computer before it joins.
	Admission string `json:"admission"`
}

// DeviceJoin tells the server whether approving this login should also add the device: Enroll for
// a device that is not a machine yet (Workspace is the destination it asks for), or the machine
// it already is, which only signs it back in.
type DeviceJoin struct {
	Enroll            bool
	EnrolledMachineID string
	Workspace         string
}

var errPublicResponseTooLarge = errors.New("paperboat-server response exceeds the size limit")

type publicCallFailure struct {
	message string
	cause   error
}

func (e *publicCallFailure) Error() string { return e.message }
func (e *publicCallFailure) Unwrap() error { return e.cause }

func DeviceAuthorize(ctx context.Context, baseURL, label, deviceType, osName string, join DeviceJoin, hc *http.Client) (DeviceAuthorization, error) {
	var out DeviceAuthorization
	body := map[string]any{"client_id": ClientID, "client_label": label, "device_type": deviceType, "os": osName, "scopes": ClientScopes}
	if join.Enroll {
		body["enroll"] = true
		if join.Workspace != "" {
			body["workspace"] = join.Workspace
		}
	} else if join.EnrolledMachineID != "" {
		body["enrolled_machine_id"] = join.EnrolledMachineID
	}
	err := publicCall(ctx, baseURL, "/v1/auth/device/authorize", body, "", &out, hc)
	return out, err
}

func RevokeToken(ctx context.Context, baseURL, token string, hc *http.Client) error {
	return publicCall(ctx, baseURL, "/v1/auth/token/revoke", nil, token, nil, hc)
}

func RefreshToken(ctx context.Context, baseURL, refreshToken, attemptID string, hc *http.Client) (TokenSet, error) {
	var out TokenSet
	err := publicCall(ctx, baseURL, "/v1/auth/token/refresh", map[string]string{"attempt_id": attemptID}, refreshToken, &out, hc)
	return out, err
}

func publicCall(ctx context.Context, baseURL, path string, body any, bearer string, out any, hc *http.Client) (callErr error) {
	var observationContext context.Context
	var hasAttempt bool
	var attemptObserved bool
	var independentFailure bool
	defer func() {
		if callErr == nil || !hasAttempt || attemptObserved && !independentFailure {
			return
		}
		errorreport.Current().ObserveFailure(observationContext, "paperboat-cli", "control_request", "control_request", "control_request_failed", callErr)
		switch failure := callErr.(type) {
		case *APIError:
			failure.cause = errorreport.RecordedHTTPFailure(failure.cause)
		case *ResponseDecodeError:
			failure.Err = errorreport.RecordedHTTPFailure(failure.Err)
		case *publicCallFailure:
			failure.cause = errorreport.RecordedHTTPFailure(failure.cause)
		}
	}()
	if hc == nil {
		hc = defaultHTTPClient()
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return &publicCallFailure{message: "paperboat-server request could not be encoded", cause: err}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return &publicCallFailure{message: "paperboat-server request could not be prepared", cause: err}
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
	req.Header.Set("User-Agent", "paperboat/"+buildinfo.Version)
	req.Header.Set("X-Paperboat-Client", ClientID)
	req.Header.Set("X-Paperboat-Protocol", buildinfo.ProtocolVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	hasAttempt = true
	resp, err := hc.Do(req)
	if err != nil {
		attemptObserved = errorreport.HTTPAttemptObserved(err)
		return &publicCallFailure{message: "paperboat-server request failed", cause: err}
	}
	statusFailure := error(nil)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusFailure = errorreport.HTTPStatusFailure(resp)
		attemptObserved = errorreport.HTTPAttemptObserved(statusFailure)
	}
	if resp.StatusCode == http.StatusNoContent && out == nil {
		if closeErr := resp.Body.Close(); closeErr != nil {
			independentFailure = true
			return &publicCallFailure{message: "paperboat-server response could not be closed", cause: closeErr}
		}
		return nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxTunnelResponseBytes+1))
	closeErr := resp.Body.Close()
	tooLarge := len(raw) > maxTunnelResponseBytes
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code             string         `json:"code"`
			Message          string         `json:"message"`
			Details          map[string]any `json:"details"`
			SupportReference string         `json:"support_reference"`
		} `json:"error"`
	}
	var decodeErr error
	if readErr == nil && !tooLarge {
		decodeErr = json.NewDecoder(bytes.NewReader(raw)).Decode(&env)
	}
	var responseCause error
	if readErr != nil {
		responseCause = errors.Join(responseCause, &ResponseDecodeError{Err: readErr})
	}
	if tooLarge {
		responseCause = errors.Join(responseCause, &ResponseDecodeError{Err: errPublicResponseTooLarge})
	}
	if decodeErr != nil {
		responseCause = errors.Join(responseCause, &ResponseDecodeError{Err: decodeErr})
	}
	if closeErr != nil {
		responseCause = errors.Join(responseCause, &publicCallFailure{message: "paperboat-server response could not be closed", cause: closeErr})
	}
	independentFailure = responseCause != nil
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUpgradeRequired || env.Error.Code == "incompatible_client_version" {
			required, _ := env.Error.Details["required_protocol"].(string)
			versionErr := &ErrIncompatibleVersion{Required: safeRequiredProtocol(required), Message: apiErrorMessage("incompatible_client_version", resp.StatusCode)}
			return &publicCallFailure{message: versionErr.Error(), cause: errors.Join(versionErr, statusFailure, responseCause)}
		}
		return &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: apiErrorMessage(env.Error.Code, resp.StatusCode), RequestID: responseRequestID(resp.Header), SupportReference: responseOrRequestSupportReference(resp.Header, env.Error.SupportReference, requestSupportReference), Details: env.Error.Details, cause: errors.Join(statusFailure, responseCause)}
	}
	if responseCause != nil {
		if readErr == nil && !tooLarge && decodeErr == nil && closeErr != nil {
			return &publicCallFailure{message: "paperboat-server response could not be closed", cause: closeErr}
		}
		return &ResponseDecodeError{Err: responseCause}
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			independentFailure = true
			return &ResponseDecodeError{Err: err}
		}
		return nil
	}
	return nil
}

func PollDeviceLogin(ctx context.Context, baseURL, deviceCode string, hc *http.Client) (out TokenSet, err error) {
	err = publicCall(ctx, baseURL, "/v1/auth/device/token", map[string]string{"client_id": ClientID, "device_code": deviceCode}, "", &out, hc)
	return
}
func CancelDeviceLogin(ctx context.Context, baseURL, deviceCode string, hc *http.Client) error {
	return publicCall(ctx, baseURL, "/v1/auth/device/cancel", map[string]string{"client_id": ClientID, "device_code": deviceCode}, "", nil, hc)
}

// StartAPITokenSession exchanges an API token for a CLI session of its creator that carries the
// token (PLAN F18-06): the token is the request's bearer and is not stored by the caller.
func StartAPITokenSession(ctx context.Context, baseURL, apiToken, label, deviceType, osName string, hc *http.Client) (out TokenSet, err error) {
	err = publicCall(ctx, baseURL, "/v1/auth/api-token-sessions", map[string]string{"client_label": label, "device_type": deviceType, "os": osName}, apiToken, &out, hc)
	return
}

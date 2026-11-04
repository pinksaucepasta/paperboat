package api

import (
	"context"
	"errors"
	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"net/http"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
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

// DeviceJoin tells the server whether approving this login should also add the device: Enroll for
// a device that is not a machine yet (Workspace is the destination it asks for), or the machine
// it already is, which only signs it back in.
type DeviceJoin struct {
	Enroll            bool
	EnrolledMachineID string
	Workspace         string
}

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
	return (&clientapi.Client{BaseURL: baseURL, Options: publicCallOptions(hc)}).RevokeToken(ctx, token)
}
func RefreshToken(ctx context.Context, baseURL, refreshToken, attemptID string, hc *http.Client) (clientapi.TokenSet, error) {
	return (&clientapi.Client{BaseURL: baseURL, Options: publicCallOptions(hc)}).RefreshToken(ctx, refreshToken, attemptID)
}

func publicCall(ctx context.Context, baseURL, path string, body any, bearer string, out any, hc *http.Client) error {
	return clientapi.Call(ctx, baseURL, http.MethodPost, path, body, bearer, out, publicCallOptions(hc))
}

func publicCallOptions(hc *http.Client) clientapi.CallOptions {
	if hc == nil {
		hc = defaultHTTPClient()
	}
	options := clientapi.CallOptions{
		HTTPClient:      hc,
		Headers:         http.Header{"User-Agent": {"paperboat/" + buildinfo.Version}, "X-Paperboat-Client": {ClientID}, "X-Paperboat-Protocol": {buildinfo.ProtocolVersion}},
		DecodeError:     func(err error) error { return &ResponseDecodeError{Err: err} },
		StatusFailure:   errorreport.HTTPStatusFailure,
		AttemptObserved: errorreport.HTTPAttemptObserved,
		ObserveFailure: func(ctx context.Context, err error) error {
			errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "control_request", "control_request", "control_request_failed", err)
			switch failure := err.(type) {
			case *APIError:
				failure.cause = errorreport.RecordedHTTPFailure(failure.cause)
			case *ResponseDecodeError:
				failure.Err = errorreport.RecordedHTTPFailure(failure.Err)
			case *clientapi.CallFailure:
				failure.Err = errorreport.RecordedHTTPFailure(failure.Err)
			}
			return err
		},
		ResponseError: func(resp *http.Response, env clientapi.Envelope, requestSupportReference string, cause error) error {
			if resp.StatusCode == http.StatusUpgradeRequired || env.Error.Code == "incompatible_client_version" {
				required, _ := env.Error.Details["required_protocol"].(string)
				versionErr := &ErrIncompatibleVersion{Required: safeRequiredProtocol(required), Message: apiErrorMessage("incompatible_client_version", resp.StatusCode)}
				return &clientapi.CallFailure{Message: versionErr.Error(), Err: errors.Join(versionErr, cause)}
			}
			return &APIError{Status: resp.StatusCode, Code: env.Error.Code, Message: apiErrorMessage(env.Error.Code, resp.StatusCode), ServerMessage: BoundedServerMessage(env.Error.Message), RequestID: responseRequestID(resp.Header), SupportReference: responseOrRequestSupportReference(resp.Header, env.Error.SupportReference, requestSupportReference), Details: env.Error.Details, cause: cause}
		},
	}
	return options
}

func PollDeviceLogin(ctx context.Context, baseURL, deviceCode string, hc *http.Client) (out clientapi.TokenSet, err error) {
	err = publicCall(ctx, baseURL, "/v1/auth/device/token", map[string]string{"client_id": ClientID, "device_code": deviceCode}, "", &out, hc)
	return
}
func CancelDeviceLogin(ctx context.Context, baseURL, deviceCode string, hc *http.Client) error {
	return publicCall(ctx, baseURL, "/v1/auth/device/cancel", map[string]string{"client_id": ClientID, "device_code": deviceCode}, "", nil, hc)
}

// StartAPITokenSession exchanges an API token for a CLI session of its creator that carries the
// token (PLAN F18-06): the token is the request's bearer and is not stored by the caller.
func StartAPITokenSession(ctx context.Context, baseURL, apiToken, label, deviceType, osName, machineID string, hc *http.Client) (clientapi.TokenSet, error) {
	return (&clientapi.Client{BaseURL: baseURL, Options: publicCallOptions(hc)}).StartAPITokenSession(ctx, apiToken, label, deviceType, osName, machineID)
}

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

var (
	ErrInvalid                 = errors.New("invalid machine bootstrap")
	ErrApprovalPending         = errors.New("machine pairing approval is pending")
	ErrPairingDenied           = errors.New("machine pairing was denied")
	ErrPairingExpired          = errors.New("machine pairing expired")
	ErrInstallationUnavailable = errors.New("machine installation material is unavailable")
)

const (
	// maxBootstrapResponseBody bounds bytes retained from a server response.
	// A malformed server cannot make the client retain an unbounded body.
	maxBootstrapResponseBody = 64 << 10
	bootstrapRequestAttempts = 3
)

type Config struct {
	ServerURL, EnrollmentToken, Alias, WorkspaceRoot, Verifier, PublicIdentityKey string
	SSHUser                                                                       string
	SSHPort                                                                       uint16
	CanReuseRuntimeIdentity                                                       bool
	RuntimeVersions                                                               map[string]string
	HTTP                                                                          *http.Client
}

type Pairing struct {
	ID        string    `json:"id"`
	UserCode  string    `json:"user_code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Material struct {
	Schema                 string          `json:"schema"`
	UserMachineID          string          `json:"user_machine_id"`
	PairingID              string          `json:"pairing_id"`
	EnvironmentID          string          `json:"environment_id"`
	ControlURL             string          `json:"control_url"`
	HelperID               string          `json:"helper_id"`
	EnrollmentID           string          `json:"enrollment_id"`
	EnrollmentCredential   string          `json:"enrollment_credential"`
	ReuseIdentity          bool            `json:"reuse_identity,omitempty"`
	ExpiresAt              time.Time       `json:"expires_at"`
	Artifact               *ArtifactTarget `json:"artifact,omitempty"`
	HelperListenAddress    string          `json:"helper_listen_address"`
	InstallationGeneration int64           `json:"installation_generation"`
	ClientSession          *ClientSession  `json:"client_session,omitempty"`
}

type ClientSession struct {
	Schema       string `json:"schema"`
	SessionID    string `json:"session_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func CreatePairing(ctx context.Context, config Config) (Pairing, error) {
	base, err := validate(config)
	if err != nil {
		return Pairing{}, err
	}
	body, err := json.Marshal(map[string]any{
		"enrollment_token": config.EnrollmentToken, "verifier": config.Verifier,
		"alias": strings.ToLower(config.Alias), "platform": runtime.GOOS, "architecture": runtime.GOARCH,
		"workspace_root": config.WorkspaceRoot, "runtime_versions": config.RuntimeVersions, "public_identity_key": config.PublicIdentityKey,
		"can_reuse_runtime_identity": config.CanReuseRuntimeIdentity,
		"ssh_user":                   strings.TrimSpace(config.SSHUser), "ssh_port": config.SSHPort,
	})
	if err != nil {
		return Pairing{}, err
	}
	var pairing Pairing
	if err := request(ctx, client(config), http.MethodPost, base+"/v1/machines/pairings", body, &pairing); err != nil {
		return Pairing{}, err
	}
	if pairing.ID == "" || pairing.UserCode == "" || !time.Now().UTC().Before(pairing.ExpiresAt) {
		return Pairing{}, bootstrapRequestFailure(ErrInvalid)
	}
	return pairing, nil
}

func WaitForMaterial(ctx context.Context, config Config, expiresAt time.Time, interval time.Duration) (Material, error) {
	base, err := validate(config)
	if err != nil {
		return Material{}, err
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	body, _ := json.Marshal(map[string]string{"verifier": config.Verifier, "public_identity_key": config.PublicIdentityKey})
	for time.Now().UTC().Before(expiresAt) {
		material, err := requestMaterial(ctx, config, base, body)
		if err == nil {
			return material, nil
		}
		if !bootstrapOnlyOutcome(err, ErrApprovalPending) && !transientBootstrapError(err) {
			return Material{}, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Material{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Material{}, ErrPairingExpired
}

// RecoverMaterial makes one verifier-bound renewal/replay request without
// applying the original pairing deadline. A protected paired resume journal
// can outlive that deadline after the server issued material but the client
// crashed before persisting or installing it.
func RecoverMaterial(ctx context.Context, config Config, runtimeEnrolled bool) (Material, error) {
	base, err := validate(config)
	if err != nil {
		return Material{}, err
	}
	body, _ := json.Marshal(map[string]any{"verifier": config.Verifier, "public_identity_key": config.PublicIdentityKey, "runtime_enrolled": runtimeEnrolled})
	return requestMaterial(ctx, config, base, body)
}

func requestMaterial(ctx context.Context, config Config, base string, body []byte) (Material, error) {
	var material Material
	if err := request(ctx, client(config), http.MethodPost, base+"/v1/machines/pairings/installation", body, &material); err != nil {
		return Material{}, err
	}
	if err := validateMaterial(material); err != nil {
		return Material{}, bootstrapRequestFailure(err)
	}
	if normalizeBootstrapURL(material.ControlURL) != normalizeBootstrapURL(config.ServerURL) {
		return Material{}, bootstrapRequestFailure(fmt.Errorf("%w: control URL does not match pairing server", ErrInvalid))
	}
	return material, nil
}

func validateMaterial(material Material) error {
	return validateMaterialFreshness(material, true)
}

// validateMaterialFreshness validates all server material fields. Resume
// loading skips only freshness so an expired, securely bound journal can ask
// the server for renewed material instead of being mistaken for corruption.
func validateMaterialFreshness(material Material, requireFresh bool) error {
	validEnrollment := material.ReuseIdentity && material.EnrollmentID == "" && material.EnrollmentCredential == "" || !material.ReuseIdentity && material.EnrollmentID != "" && len(material.EnrollmentCredential) >= 32
	validClientSession := material.ClientSession != nil && validBootstrapClientSession(*material.ClientSession)
	checks := []struct {
		invalid bool
		reason  string
	}{
		{material.Schema != "paperboat.machine-installation/v1", "schema"},
		{material.UserMachineID == "", "user machine id"},
		{material.PairingID == "", "enrollment id"},
		{material.EnvironmentID == "", "environment id"},
		{material.HelperID == "", "helper id"},
		{!validBootstrapControlURL(material.ControlURL), "control URL"},
		{!validEnrollment, "enrollment credential"},
		{!validLoopbackAddress(material.HelperListenAddress), "helper listen address"},
		{material.InstallationGeneration < 1, "installation generation"},
		{!validClientSession, "client session"},
		{requireFresh && !time.Now().UTC().Before(material.ExpiresAt), "expiration"},
		{material.Artifact == nil, "artifact"},
	}
	for _, check := range checks {
		if check.invalid {
			return fmt.Errorf("%w: %s", ErrInvalid, check.reason)
		}
	}
	if err := VerifyArtifactTarget(*material.Artifact); err != nil {
		return fmt.Errorf("%w: artifact target: %w", ErrInvalid, err)
	}
	return nil
}

func validBootstrapControlURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.User == nil && parsed.Hostname() != "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func normalizeBootstrapURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func validBootstrapClientSession(session ClientSession) bool {
	return session.Schema == "paperboat.cli-session/v1" && boundedBootstrapValue(session.SessionID, 1, 256) &&
		boundedBootstrapValue(session.AccessToken, 32, 16<<10) && boundedBootstrapValue(session.RefreshToken, 32, 16<<10) &&
		session.TokenType == "Bearer" && session.ExpiresIn > 0 && session.ExpiresIn <= 7*24*60*60 &&
		(len(session.Scope) == 0 || boundedBootstrapValue(session.Scope, 1, 4<<10))
}

func boundedBootstrapValue(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && !strings.ContainsAny(value, "\x00\r\n")
}

// transientBootstrapError reports errors that do not carry pairing-terminal
// meaning: stalled or reset connections and timeouts. Approval polling must
// survive them instead of abandoning a pairing that is still redeemable.
func transientBootstrapError(err error) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if _, duplicate := seen[current]; duplicate {
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(remaining) > 15-visited {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			remaining = append(remaining, wrapped.Unwrap())
		default:
			if transientBootstrapLeaf(current) {
				leaves++
				continue
			}
			return false
		}
	}
	return leaves > 0
}

func transientBootstrapLeaf(err error) bool {
	if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
		return true
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno == syscall.ECONNREFUSED || errno == syscall.ECONNRESET || errno == syscall.ECONNABORTED || errno == syscall.EPIPE
	}
	return false
}

func validLoopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func ValidateWorkspace(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return ErrInvalid
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("%w: inspect workspace: %w", ErrInvalid, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalid
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("%w: resolve workspace: %w", ErrInvalid, err)
	}
	if resolved != root {
		return ErrInvalid
	}
	return nil
}

func validate(config Config) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.ServerURL))
	publicKey, keyErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(config.PublicIdentityKey))
	token := strings.TrimSpace(config.EnrollmentToken)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" || token != "" && (len(token) < 26 || len(token) > 256) || len(config.Verifier) < 32 || strings.TrimSpace(config.Alias) == "" || keyErr != nil || len(publicKey) != ed25519.PublicKeySize {
		return "", ErrInvalid
	}
	if err := ValidateWorkspace(config.WorkspaceRoot); err != nil {
		return "", err
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func client(config Config) *http.Client {
	base := config.HTTP
	if base == nil {
		base = &http.Client{Transport: httptransport.Default(), Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
	}
	copyClient := *base
	copyClient.Transport = errorreport.TransportOperation(base.Transport, config.ServerURL, "machine_pairing")
	return &copyClient
}

func request(ctx context.Context, client *http.Client, method, target string, body []byte, output any) error {
	var response *http.Response
	for attempt := 0; attempt < bootstrapRequestAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return bootstrapRequestFailure(err)
		}
		request.Header.Set("Content-Type", "application/json")
		var wroteRequest atomic.Bool
		trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest.Store(true) }}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
		response, err = client.Do(request)
		if err == nil {
			break
		}
		closeErr := closeBootstrapResponse(response)
		attemptErr := joinBootstrapErrors(err, closeErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if closeErr == nil && bootstrapOnlyCause(err, ctxErr) {
				return ctxErr
			}
			return bootstrapRequestFailure(joinBootstrapErrors(ctxErr, attemptErr))
		}
		if closeErr != nil {
			return bootstrapRequestFailure(attemptErr)
		}
		// A dashboard token is single-use and CreatePairing is not itself
		// idempotent. Retry only while net/http proves that no request bytes
		// reached the connection. Once WroteRequest fires, the protected
		// verifier resume flow owns uncertain-outcome recovery.
		if wroteRequest.Load() || !transientBootstrapError(err) || attempt+1 == bootstrapRequestAttempts {
			return bootstrapRequestFailure(err)
		}
		delay := 250 * time.Millisecond << attempt
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxBootstrapResponseBody+1))
	defer clearBytes(encoded)
	if err != nil {
		cause := err
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			cause = joinBootstrapErrors(errorreport.HTTPStatusFailure(response), cause)
		}
		closeErr := closeBootstrapResponse(response)
		failure := joinBootstrapErrors(cause, closeErr)
		observeBootstrapFailure(ctx, joinBootstrapErrors(err, closeErr))
		return bootstrapRequestFailure(failure)
	}
	if len(encoded) > maxBootstrapResponseBody {
		closeErr := closeBootstrapResponse(response)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			failure := bootstrapServerError(ErrInvalid, response)
			if closeErr != nil {
				failure = bootstrapRequestFailure(joinBootstrapErrors(failure, closeErr))
			}
			return failure
		}
		return bootstrapRequestFailure(joinBootstrapErrors(ErrInvalid, closeErr))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(encoded, &envelope)
		var failure error
		switch envelope.Error.Code {
		case "machine_approval_pending", "user_machine_approval_pending":
			failure = bootstrapServerError(ErrApprovalPending, response)
		case "machine_pairing_denied", "user_machine_pairing_denied":
			failure = bootstrapServerError(ErrPairingDenied, response)
		case "machine_pairing_expired", "user_machine_pairing_expired":
			failure = bootstrapServerError(ErrPairingExpired, response)
		case "machine_installation_unavailable", "user_machine_installation_unavailable":
			failure = bootstrapServerError(ErrInstallationUnavailable, response)
		default:
			failure = bootstrapServerError(ErrInvalid, response)
		}
		closeErr := closeBootstrapResponse(response)
		if closeErr != nil {
			failure = bootstrapRequestFailure(joinBootstrapErrors(failure, closeErr))
		}
		return failure
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		closeErr := closeBootstrapResponse(response)
		return bootstrapRequestFailure(joinBootstrapErrors(errors.Join(ErrInvalid, err), closeErr))
	}
	if len(envelope.Data) == 0 {
		closeErr := closeBootstrapResponse(response)
		return bootstrapRequestFailure(joinBootstrapErrors(ErrInvalid, closeErr))
	}
	if err := json.Unmarshal(envelope.Data, output); err != nil {
		closeErr := closeBootstrapResponse(response)
		return bootstrapRequestFailure(joinBootstrapErrors(errors.Join(ErrInvalid, err), closeErr))
	}
	if err := closeBootstrapResponse(response); err != nil {
		observeBootstrapFailure(ctx, err)
		return bootstrapRequestFailure(err)
	}
	return nil
}

type bootstrapRequestError struct {
	kind  error
	cause error
}

func (failure bootstrapRequestError) Error() string {
	if failure.kind != nil {
		var status interface{ DiagnosticStatus() int }
		if errors.As(failure.cause, &status) {
			return fmt.Sprintf("%s (HTTP %d)", failure.kind.Error(), status.DiagnosticStatus())
		}
		return failure.kind.Error()
	}
	return "machine pairing request failed"
}

func (failure bootstrapRequestError) Unwrap() error { return failure.cause }

func (failure bootstrapRequestError) Is(target error) bool {
	return sameBootstrapError(failure.kind, target)
}

func (bootstrapRequestError) DiagnosticStage() string         { return "control_request" }
func (bootstrapRequestError) DiagnosticCode() string          { return "control_request_failed" }
func (failure bootstrapRequestError) bootstrapOutcome() error { return failure.kind }

func bootstrapRequestFailure(cause error) error {
	return bootstrapRequestError{cause: cause}
}

func bootstrapServerError(kind error, response *http.Response) error {
	return bootstrapRequestError{kind: kind, cause: errorreport.HTTPStatusFailure(response)}
}

func closeBootstrapResponse(response *http.Response) error {
	if response == nil || response.Body == nil {
		return nil
	}
	return response.Body.Close()
}

func joinBootstrapErrors(primary, cleanup error) error {
	if primary == nil {
		return cleanup
	}
	if cleanup == nil || sameBootstrapError(primary, cleanup) {
		return primary
	}
	return errors.Join(primary, cleanup)
}

func sameBootstrapError(left, right error) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftType, rightType := reflect.TypeOf(left), reflect.TypeOf(right)
	return leftType == rightType && leftType.Comparable() && left == right
}

func observeBootstrapFailure(ctx context.Context, err error) {
	if err == nil || errorreport.HTTPAttemptObserved(err) {
		return
	}
	errorreport.Current().ObserveFailure(ctx, "paperboat-cli", "machine_pairing", "control_request", "control_request_failed", err)
}

func bootstrapOnlyCause(err, expected error) bool {
	return bootstrapAllLeavesMatch(err, func(leaf error) bool { return sameBootstrapError(leaf, expected) })
}

func bootstrapAllLeavesMatch(err error, matches func(error) bool) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if _, duplicate := seen[current]; duplicate {
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(remaining) > 15-visited {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				return false
			}
			remaining = append(remaining, child)
		default:
			leaves++
			if !matches(current) {
				return false
			}
		}
	}
	return leaves > 0
}

func bootstrapOnlyOutcome(err, expected error) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	outcomes := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if value.Type().Comparable() {
			if _, duplicate := seen[current]; duplicate {
				return false
			}
			seen[current] = struct{}{}
		}
		if failure, ok := current.(interface{ bootstrapOutcome() error }); ok {
			if !sameBootstrapError(failure.bootstrapOutcome(), expected) {
				return false
			}
			outcomes++
			continue
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(remaining) > 15-visited {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				return false
			}
			remaining = append(remaining, child)
		default:
			return false
		}
	}
	return outcomes > 0
}

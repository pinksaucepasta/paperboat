package machinecontrol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

var (
	ErrInvalid     = errors.New("machine control credential is invalid")
	ErrUnavailable = errors.New("machine control credentials are unavailable")

	errMissingResponseBody      = errors.New("machine control response body is missing")
	errResponseTooLarge         = errors.New("machine control response exceeds its size limit")
	errInvalidMachineControlDoc = errors.New("machine control response is invalid")
	errControlRedirect          = errors.New("machine control redirect refused")
)

const machineControlResponseLimit = 32 << 10

type sourceFailure struct{ cause error }

func (sourceFailure) Error() string         { return ErrUnavailable.Error() }
func (failure sourceFailure) Unwrap() error { return failure.cause }
func (failure sourceFailure) Is(target error) bool {
	return target == ErrUnavailable || errors.Is(failure.cause, target)
}

func unavailable(cause error) error {
	if cause == nil {
		return ErrUnavailable
	}
	return sourceFailure{cause: cause}
}

// expectedInvalidMachineControl accepts only a bounded tree made entirely of
// the identity package's invalid-store and not-found results. Operational or
// parser causes must not turn a local storage failure into authentication
// rejection.
func expectedInvalidMachineControl(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
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
			if _, exists := seen[current]; exists {
				return false
			}
			seen[current] = struct{}{}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(pending)+len(children) > 16-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			if child := wrapped.Unwrap(); child != nil {
				pending = append(pending, child)
				continue
			}
			if !allowedMachineControlInvalidLeaf(current) {
				return false
			}
		default:
			if !allowedMachineControlInvalidLeaf(current) {
				return false
			}
		}
	}
	return true
}

func allowedMachineControlInvalidLeaf(err error) bool {
	return errors.Is(err, identity.ErrInvalidStore) || errors.Is(err, os.ErrNotExist)
}

func readMachineControlResponse(response *http.Response) ([]byte, bool, error) {
	if response == nil || response.Body == nil {
		return nil, false, errMissingResponseBody
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, machineControlResponseLimit+1))
	closeErr := response.Body.Close()
	tooLarge := len(raw) > machineControlResponseLimit
	if tooLarge {
		readErr = errors.Join(readErr, errResponseTooLarge)
	}
	return raw, tooLarge, errors.Join(readErr, closeErr)
}

func decodeMachineControlResponse(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return err
		}
		return errInvalidMachineControlDoc
	}
	return nil
}

type Config struct {
	ControlURL  string
	StateRoot   string
	Transport   http.RoundTripper
	Timeout     time.Duration
	RenewBefore time.Duration
	Clock       func() time.Time
	OperationID func() (string, error)
}

type Source struct {
	config   Config
	endpoint *url.URL
	client   *http.Client
	mu       sync.Mutex
}

func NewSource(config Config) (*Source, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.ControlURL))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || config.StateRoot == "" {
		return nil, ErrInvalid
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/machine-control-renewals"
	if config.Timeout <= 0 || config.Timeout > 30*time.Second {
		config.Timeout = 15 * time.Second
	}
	if config.RenewBefore <= 0 || config.RenewBefore >= time.Hour {
		config.RenewBefore = 10 * time.Minute
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.OperationID == nil {
		config.OperationID = randomOperationID
	}
	return &Source{config: config, endpoint: endpoint, client: &http.Client{Transport: errorreport.TransportOperation(config.Transport, endpoint.String(), "machine_control"), Timeout: config.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errControlRedirect }}}, nil
}

func (s *Source) Token(ctx context.Context) (string, error) {
	if s == nil || ctx == nil {
		return "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := identity.Open(identity.Config{StateRoot: s.config.StateRoot})
	if err != nil {
		return "", unavailable(err)
	}
	now := s.config.Clock().UTC()
	// Renewal must authenticate with a credential that is valid now. The
	// renewal window is applied below; an expiry grace period here could send
	// an already expired credential to the control plane.
	current, err := store.MachineControl(now, 0)
	if err != nil {
		if expectedInvalidMachineControl(err) {
			return "", ErrInvalid
		}
		return "", unavailable(err)
	}
	if current.ExpiresAt.After(now.Add(s.config.RenewBefore)) {
		return current.Credential, nil
	}
	operationID, err := s.config.OperationID()
	if err != nil {
		return "", unavailable(err)
	}
	if len(operationID) < 8 || len(operationID) > 128 {
		return "", ErrInvalid
	}
	body, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
	}{operationID})
	if err != nil {
		return "", unavailable(err)
	}
	proof, err := store.MachineProof(operationID, http.MethodPost, s.endpoint.Path, body, now)
	if err != nil {
		return "", unavailable(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", unavailable(err)
	}
	request.Header.Set("Authorization", "Bearer "+current.Credential)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", unavailable(err)
	}
	raw, tooLarge, bodyErr := readMachineControlResponse(response)
	if response.StatusCode != http.StatusCreated {
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && bodyErr == nil && !tooLarge {
			return "", ErrInvalid
		}
		statusFailure := errorreport.HTTPStatusFailure(response)
		if bodyErr != nil {
			statusFailure = errors.Join(statusFailure, bodyErr)
		}
		return "", unavailable(statusFailure)
	}
	if bodyErr != nil || tooLarge {
		return "", unavailable(bodyErr)
	}
	var envelope struct {
		Data identity.MachineControl `json:"data"`
	}
	if err := decodeMachineControlResponse(raw, &envelope); err != nil {
		return "", unavailable(err)
	}
	envelope.Data.MachineID = current.MachineID
	envelope.Data.EnvironmentID = current.EnvironmentID
	envelope.Data.InstallationGeneration = current.InstallationGeneration
	envelope.Data.KeyID = current.KeyID
	if len(envelope.Data.Credential) < 32 || !envelope.Data.ExpiresAt.After(now) {
		return "", unavailable(errInvalidMachineControlDoc)
	}
	if err := store.SaveMachineControl(envelope.Data); err != nil {
		return "", unavailable(err)
	}
	return envelope.Data.Credential, nil
}

// EnsureInitial obtains the first machine-control credential after a helper
// identity has been enrolled. The helper identity proves possession of the
// same machine key that the control plane bound during enrollment; it is not
// written into machine-control.json or used as a machine-control credential.
//
// The operation ID is deterministic for one machine key and installation
// generation. If the server commits before this process writes the local file,
// a restart receives the exact same credential rather than minting another.
func (s *Source) EnsureInitial(ctx context.Context) (string, error) {
	if s == nil || ctx == nil {
		return "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store, err := identity.Open(identity.Config{StateRoot: s.config.StateRoot})
	if err != nil {
		return "", unavailable(err)
	}
	now := s.config.Clock().UTC()
	if current, currentErr := store.MachineControl(now, 0); currentErr == nil {
		return current.Credential, nil
	} else if !expectedInvalidMachineControl(currentErr) {
		return "", unavailable(currentErr)
	}
	registration, err := store.Registration()
	if err != nil {
		return "", unavailable(err)
	}
	operationID := initialOperationID(registration, store.Current().ID)
	body, err := json.Marshal(struct {
		OperationID string `json:"operation_id"`
	}{operationID})
	if err != nil {
		return "", unavailable(err)
	}
	runtimeTokens := enrollment.TokenSource{StateRoot: s.config.StateRoot, Clock: s.config.Clock}
	runtimeProofs := enrollment.ProofSource{StateRoot: s.config.StateRoot, Clock: s.config.Clock}
	token, err := runtimeTokens.Token(ctx)
	if err != nil {
		return "", unavailable(err)
	}
	path := "/v1/machine-control-credentials"
	proof, err := runtimeProofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return "", unavailable(err)
	}
	endpoint := *s.endpoint
	endpoint.Path = strings.TrimSuffix(s.endpoint.Path, "/v1/machine-control-renewals") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", unavailable(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", unavailable(err)
	}
	raw, tooLarge, bodyErr := readMachineControlResponse(response)
	if response.StatusCode != http.StatusCreated {
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && bodyErr == nil && !tooLarge {
			return "", ErrInvalid
		}
		statusFailure := errorreport.HTTPStatusFailure(response)
		if bodyErr != nil {
			statusFailure = errors.Join(statusFailure, bodyErr)
		}
		return "", unavailable(statusFailure)
	}
	if bodyErr != nil || tooLarge {
		return "", unavailable(bodyErr)
	}
	var envelope struct {
		Data identity.MachineControl `json:"data"`
	}
	if err := decodeMachineControlResponse(raw, &envelope); err != nil {
		return "", unavailable(err)
	}
	if len(envelope.Data.Credential) < 32 || !envelope.Data.ExpiresAt.After(now) {
		return "", unavailable(errInvalidMachineControlDoc)
	}
	envelope.Data.MachineID = registration.MachineID
	envelope.Data.EnvironmentID = registration.EnvironmentID
	envelope.Data.InstallationGeneration = registration.InstallationGeneration
	envelope.Data.KeyID = store.Current().ID
	if err := store.SaveMachineControl(envelope.Data); err != nil {
		return "", unavailable(err)
	}
	return envelope.Data.Credential, nil
}

func (s *Source) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	method = strings.ToUpper(method)
	if s == nil || len(operationID) < 8 || len(operationID) > 128 || method != http.MethodPost && method != http.MethodPut && method != http.MethodDelete || !strings.HasPrefix(path, "/v1/") || len(body) > 1<<20 {
		return nil, ErrInvalid
	}
	store, err := identity.Open(identity.Config{StateRoot: s.config.StateRoot})
	if err != nil {
		return nil, unavailable(err)
	}
	proof, err := store.MachineProof(operationID, method, path, body, s.config.Clock().UTC())
	if err != nil {
		return nil, unavailable(err)
	}
	return proof, nil
}

func randomOperationID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "operation_" + id.String(), nil
}

func initialOperationID(registration identity.Registration, keyID string) string {
	digest := sha256.Sum256([]byte(registration.MachineID + "\x00" + registration.EnvironmentID + "\x00" + strconv.FormatInt(registration.InstallationGeneration, 10) + "\x00" + keyID))
	return "machine-control-initial-" + base64.RawURLEncoding.EncodeToString(digest[:])
}

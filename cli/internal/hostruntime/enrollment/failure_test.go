package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type enrollmentPrivateCause struct{}

func (*enrollmentPrivateCause) Error() string { panic("private enrollment cause formatted") }

type enrollmentFailureBody struct {
	io.Reader
	readErr, closeErr error
	closes            int
}

func (body *enrollmentFailureBody) Read(value []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	return body.Reader.Read(value)
}
func (body *enrollmentFailureBody) Close() error { body.closes++; return body.closeErr }

func TestEnrollmentExhaustionRetainsLastCauseWithoutFormatting(t *testing.T) {
	spy := &enrollmentPrivateCause{}
	cause := errors.Join(syscall.EIO, spy)
	var attempts atomic.Int32
	client, err := NewClient(enrollmentRoundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, cause
	}), time.Second)
	if err != nil {
		t.Fatal("client construction failed")
	}
	_, err = client.Enroll(t.Context(), Config{ControlURL: "https://control.example.test", StateRoot: t.TempDir(), EnrollmentCredential: strings.Repeat("x", 32)})
	if !errors.Is(err, ErrEnrollmentExchangeUnavailable) || !errors.Is(err, syscall.EIO) || !errors.Is(err, spy) || errors.Is(err, ErrEnrollmentExchangeRejected) || attempts.Load() != enrollmentExchangeAttempts {
		t.Fatal("exhaustion lost its original cause or became rejection")
	}
	if err.Error() != ErrEnrollmentExchangeUnavailable.Error() {
		t.Fatal("unsafe exhaustion presentation")
	}
}

func TestEnrollmentResponseReadAndCloseFailuresCannotBecomeDenial(t *testing.T) {
	body := &enrollmentFailureBody{Reader: strings.NewReader("PRIVATE-RESPONSE"), readErr: syscall.EIO, closeErr: syscall.ENOSPC}
	client, _ := NewClient(nil, time.Second)
	httpClient := &http.Client{Transport: enrollmentRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: body}, nil
	})}
	status, raw, err := client.enrollmentExchangeAttempt(t.Context(), httpClient, "https://control.example.test", []byte(`{}`))
	if status != http.StatusForbidden || raw != nil || body.closes != 1 || !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("failed response cleanup lost original causes or returned payload")
	}
}

func TestRuntimeIdentityParserAndMissingFileCausesAreSafe(t *testing.T) {
	root := t.TempDir()
	_, err := LoadRuntimeIdentity(root, time.Now().UTC())
	var missing *os.PathError
	if !errors.As(err, &missing) || !errors.Is(err, os.ErrNotExist) || err.Error() != ErrUnavailable.Error() || strings.Contains(err.Error(), root) {
		t.Fatal("missing identity lost its safe native cause")
	}
	path := filepath.Join(root, "runtime-identity.json")
	if err := os.WriteFile(path, []byte(`{"credential":! PRIVATE-CREDENTIAL}`), 0o600); err != nil {
		t.Fatal("fixture write failed")
	}
	_, err = LoadRuntimeIdentity(root, time.Now().UTC())
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) || err.Error() != ErrUnavailable.Error() {
		t.Fatal("identity parser cause was lost or formatted")
	}
}

func TestRenewalOutageRetainsStatusAndFreshAttemptRecovers(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/helper-identity-renewals" && fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "helper_1", "machine_id": "machine_1", "environment_id": "environment_1", "credential": strings.Repeat("r", 40), "expires_at": now.Add(time.Hour)}})
	}))
	defer server.Close()
	root := t.TempDir()
	client, _ := NewClient(server.Client().Transport, time.Second)
	current, err := client.Enroll(t.Context(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: strings.Repeat("e", 40)})
	if err != nil {
		t.Fatal("initial enrollment failed")
	}
	current.ExpiresAt = now.Add(time.Minute)
	if err := writeIdentity(root, current); err != nil {
		t.Fatal("identity fixture write failed")
	}
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, Clock: func() time.Time { return now }, OperationID: func() (string, error) { return "operation-renew-fixture", nil }})
	if err != nil {
		t.Fatal("renewal source construction failed")
	}
	_, err = source.Token(context.Background())
	if err == nil || errors.Is(err, ErrInvalid) || errorreport.ProjectFault(t.Context(), "paperboat-daemon", "identity_renewal", "control_request", "control_request_failed", err).HTTPStatus != http.StatusServiceUnavailable || err.Error() != ErrUnavailable.Error() {
		t.Fatal("renewal outage became invalid enrollment or lost safe status")
	}
	fail.Store(false)
	if token, err := source.Token(t.Context()); err != nil || token != strings.Repeat("r", 40) {
		t.Fatal("fresh renewal did not recover")
	}
	stored, err := LoadRuntimeIdentity(root, now)
	if err != nil || !stored.ExpiresAt.After(current.ExpiresAt) {
		t.Fatal("recovered identity was not persisted")
	}
}

type enrollmentFailedMetrics struct{ cause error }

func (metrics enrollmentFailedMetrics) Record(string, float64, map[string]string) error {
	return metrics.cause
}

func TestRenewalMetricFailureIsObservedWithoutReplacingPrimaryCause(t *testing.T) {
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	ctx := supportref.WithContext(t.Context(), reference)
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restore()
	source, err := NewRenewingTokenSource(RenewingTokenConfig{
		ControlURL: "https://control.example.test", StateRoot: t.TempDir(), Transport: http.DefaultTransport,
		OperationID: func() (string, error) { return "operation-unused", nil },
		Metrics:     enrollmentFailedMetrics{cause: errors.Join(syscall.ENOSPC, &enrollmentPrivateCause{})},
	})
	if err != nil {
		t.Fatal("source construction failed")
	}
	_, err = source.Token(ctx)
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOSPC) {
		t.Fatal("diagnostic failure replaced primary storage cause")
	}
	if len(faults) != 1 || faults[0].Stage != "diagnostic_storage" || faults[0].Errno != int(syscall.ENOSPC) || faults[0].SupportReference != reference {
		t.Fatal("metric failure lost its safe correlated observation")
	}
}

package selfhost

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

type requestNodeRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip requestNodeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type requestNodeBody struct {
	reader   io.Reader
	closeErr error
}

func (body *requestNodeBody) Read(p []byte) (int, error) { return body.reader.Read(p) }
func (body *requestNodeBody) Close() error               { return body.closeErr }

type requestNodeErrorReader struct{ err error }

func (reader requestNodeErrorReader) Read([]byte) (int, error) { return 0, reader.err }

func requestNodeTestClient(roundTrip requestNodeRoundTripper) *http.Client {
	return &http.Client{Transport: roundTrip}
}

func requestNodeTestResponse(request *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: body, Request: request}
}

func invokeRequestNode(client *http.Client, out any) error {
	return requestNode(context.Background(), client, "https://node.invalid/v1/selfhost/inspect", "test-secret", map[string]string{"nonce": "test"}, out)
}

func TestRequestNodePreservesCleanExpectedStatuses(t *testing.T) {
	for _, test := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, ErrUnauthorized}, {http.StatusForbidden, ErrUnauthorized}, {http.StatusConflict, ErrUsed}, {http.StatusGone, ErrExpired}} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			client := requestNodeTestClient(func(request *http.Request) (*http.Response, error) {
				body := &requestNodeBody{reader: strings.NewReader("")}
				return requestNodeTestResponse(request, test.status, body), nil
			})
			err := invokeRequestNode(client, new(map[string]any))
			if !errors.Is(err, test.want) {
				t.Fatalf("status %d lost expected outcome %v: %v", test.status, test.want, err)
			}
			var failure *requestNodeFailure
			if errors.As(err, &failure) {
				t.Fatalf("clean status %d became an operational failure: %v", test.status, err)
			}
		})
	}
}

func TestRequestNodeKeepsTransportAndBodyReadCausesPrivate(t *testing.T) {
	for _, test := range []struct {
		name string
	}{
		{name: "transport"},
		{name: "body read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			private := errors.New("REQUEST_NODE_PRIVATE_CAUSE")
			var client *http.Client
			if test.name == "transport" {
				client = requestNodeTestClient(func(*http.Request) (*http.Response, error) { return nil, private })
			} else {
				client = requestNodeTestClient(func(request *http.Request) (*http.Response, error) {
					return requestNodeTestResponse(request, http.StatusOK, &requestNodeBody{reader: requestNodeErrorReader{err: private}}), nil
				})
			}
			err := invokeRequestNode(client, new(map[string]any))
			if !errors.Is(err, private) {
				t.Fatal("request failure lost its original cause")
			}
			assertRequestNodeCausePrivate(t, err, private.Error())
		})
	}
}

func TestRequestNodeKeepsDecodeTrailingStatusAndCloseEvidence(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		close  error
		cause  error
	}{
		{name: "malformed response", status: http.StatusOK, body: `{"value":`},
		{name: "trailing response data", status: http.StatusOK, body: `{"value":true} {"secret":"NODE_PRIVATE_TRAILING"}`, cause: errTrailingNodeResponse},
		{name: "close after success", status: http.StatusOK, body: `{"value":true}`, close: errors.New("NODE_PRIVATE_CLOSE")},
		{name: "clean nonexpected status", status: http.StatusInternalServerError, body: `NODE_PRIVATE_STATUS`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &requestNodeBody{reader: strings.NewReader(test.body), closeErr: test.close}
			client := requestNodeTestClient(func(request *http.Request) (*http.Response, error) {
				return requestNodeTestResponse(request, test.status, body), nil
			})
			err := invokeRequestNode(client, new(map[string]any))
			if err == nil {
				t.Fatal("invalid response unexpectedly succeeded")
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("response lost cause %v", test.cause)
			}
			if test.close != nil && !errors.Is(err, test.close) {
				t.Fatal("response close failure was discarded")
			}
			var failure *requestNodeFailure
			if !errors.As(err, &failure) {
				t.Fatalf("operational response was not typed: %T", err)
			}
			if failure.status != test.status {
				t.Fatalf("response status=%d, want %d", failure.status, test.status)
			}
			fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "selfhost_enrollment", "service_error", "selfhost_unavailable", err)
			if fault.HTTPStatus != test.status && test.status >= 400 {
				t.Fatalf("projected HTTP status=%d, want %d", fault.HTTPStatus, test.status)
			}
			assertRequestNodeCausePrivate(t, err, "NODE_PRIVATE_")
		})
	}
}

func TestRequestNodeExpectedStatusWithCloseFailureIsOperational(t *testing.T) {
	private := errors.New("NODE_PRIVATE_401_CLOSE")
	client := requestNodeTestClient(func(request *http.Request) (*http.Response, error) {
		return requestNodeTestResponse(request, http.StatusUnauthorized, &requestNodeBody{reader: strings.NewReader(""), closeErr: private}), nil
	})
	err := invokeRequestNode(client, new(map[string]any))
	if !errors.Is(err, ErrUnauthorized) || !errors.Is(err, private) {
		t.Fatal("mixed unauthorized response and body-close failure lost one cause")
	}
	var failure *requestNodeFailure
	if !errors.As(err, &failure) || failure.status != http.StatusUnauthorized {
		t.Fatal("mixed response did not retain its status")
	}
	assertRequestNodeCausePrivate(t, err, private.Error())
}

func TestDatabaseCleanupFailureKeepsPrimaryAndCleanupCauses(t *testing.T) {
	rollbackErr := syscall.EIO
	got := withTransactionRollbackFailure("remove_installation", ErrUnauthorized, rollbackErr)
	if !errors.Is(got, ErrUnauthorized) || !errors.Is(got, rollbackErr) {
		t.Fatal("rollback failure did not retain both the business and cleanup causes")
	}
	if strings.Contains(got.Error(), rollbackErr.Error()) {
		t.Fatal("database cleanup error exposed driver prose")
	}
	fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "selfhost_remove", "service_error", "selfhost_unavailable", got)
	if fault.Cause != "system_error" || fault.Errno != int(syscall.EIO) {
		t.Fatalf("mixed transaction failure lost operational cause: %+v", fault)
	}
	primary := ErrUnauthorized
	if unchanged := withTransactionRollbackFailure("remove_installation", primary, sql.ErrTxDone); unchanged != primary {
		t.Fatal("exact completed-transaction sentinel was not suppressed")
	}
	wrappedDone := fmt.Errorf("wrapped transaction completion: %w", sql.ErrTxDone)
	if retained := withTransactionRollbackFailure("remove_installation", primary, wrappedDone); retained == primary || !errors.Is(retained, sql.ErrTxDone) {
		t.Fatal("non-sentinel cleanup failure was suppressed")
	}
	rowClose := withDatabaseCleanupFailure("list_installations", ErrLimit, rollbackErr)
	if !errors.Is(rowClose, ErrLimit) || !errors.Is(rowClose, rollbackErr) {
		t.Fatal("row cleanup failure did not retain both pagination and cleanup causes")
	}
}

func assertRequestNodeCausePrivate(t *testing.T, err error, secret string) {
	t.Helper()
	if strings.Contains(err.Error(), secret) {
		t.Fatal("request error formatted private response or transport text")
	}
	fault := supportreport.ProjectFault(context.Background(), "paperboat-server", "selfhost_enrollment", "service_error", "selfhost_unavailable", err)
	encoded, marshalErr := json.Marshal(fault)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("projected request fault exposed private response or transport text")
	}
}

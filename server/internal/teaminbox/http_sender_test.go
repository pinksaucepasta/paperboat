package teaminbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type receiptRoundTripFunc func(*http.Request) (*http.Response, error)

func (f receiptRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type receiptResponseBody struct {
	reader   io.Reader
	closeErr error
}

func (b receiptResponseBody) Read(raw []byte) (int, error) { return b.reader.Read(raw) }
func (b receiptResponseBody) Close() error                 { return b.closeErr }

func TestHTTPReceiptSenderUsesIdempotencyAndMinimalBody(t *testing.T) {
	var body map[string]string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Idempotency-Key") != "tir_1" {
			t.Errorf("headers=%v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "sink_1"})
	}))
	defer sink.Close()
	sender, err := NewHTTPReceiptSender(sink.URL, "test-token", "receipts@example.invalid", sink.Client())
	if err != nil {
		t.Fatal(err)
	}
	id, err := sender.SendReceipt(context.Background(), ReceiptMessage{IdempotencyKey: "tir_1", To: "recipient@example.invalid", Subject: "Files received", Text: "Paperboat received 2 files."})
	if err != nil || id != "sink_1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if len(body) != 4 || body["to"] != "recipient@example.invalid" || body["text"] != "Paperboat received 2 files." {
		t.Fatalf("body=%v", body)
	}
}

func TestHTTPReceiptSenderRejectsRemotePlaintextAndHeaderInjection(t *testing.T) {
	if _, err := NewHTTPReceiptSender("http://email.example.test/send", "test-token", "receipts@example.invalid", nil); err == nil {
		t.Fatal("remote plaintext endpoint accepted")
	}
	sender, err := NewHTTPReceiptSender("https://email.example.test/send", "test-token", "receipts@example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sender.SendReceipt(context.Background(), ReceiptMessage{IdempotencyKey: "tir_1", To: "victim@example.invalid\r\nBcc: other@example.invalid", Subject: "x", Text: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("injection error=%v", err)
	}
}

func TestHTTPReceiptSenderKeepsPrivateProviderFailuresStructured(t *testing.T) {
	readCause := errors.New("SECRET provider body read")
	closeCause := errors.New("SECRET provider body close")
	transport := receiptRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: receiptResponseBody{reader: readFailure{err: readCause}, closeErr: closeCause}}, nil
	})
	sender, err := NewHTTPReceiptSender("https://email.example.test/send", "test-token", "receipts@example.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	message := ReceiptMessage{IdempotencyKey: "tir_1", To: "recipient@example.invalid", Subject: "Files received", Text: "Paperboat received 2 files."}
	_, err = sender.SendReceipt(context.Background(), message)
	if !errors.Is(err, readCause) || !errors.Is(err, closeCause) || err.Error() != "receipt email delivery failed" {
		t.Fatalf("provider read/close errors were lost or exposed: %v", err)
	}
	transport = receiptRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: receiptResponseBody{reader: bytes.NewReader([]byte("not-json"))}}, nil
	})
	sender, err = NewHTTPReceiptSender("https://email.example.test/send", "test-token", "receipts@example.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.SendReceipt(context.Background(), message)
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || err.Error() != "receipt email response is invalid" || bytes.Contains([]byte(err.Error()), []byte("not-json")) {
		t.Fatalf("provider decode error was lost or exposed: %v", err)
	}
}

func TestHTTPReceiptSenderRetainsAcceptedIDWhenBodyCloseFails(t *testing.T) {
	closeCause := errors.New("SECRET accepted response close")
	transport := receiptRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusAccepted, Body: receiptResponseBody{reader: bytes.NewReader([]byte(`{"message_id":"accepted_1"}`)), closeErr: closeCause}}, nil
	})
	sender, err := NewHTTPReceiptSender("https://email.example.test/send", "test-token", "receipts@example.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	id, err := sender.SendReceipt(context.Background(), ReceiptMessage{IdempotencyKey: "tir_1", To: "recipient@example.invalid", Subject: "Files received", Text: "Paperboat received 2 files."})
	if id != "accepted_1" || !errors.Is(err, closeCause) || err.Error() != "receipt email delivery failed" {
		t.Fatalf("confirmed provider ID or private close cause was lost: id=%q err=%v", id, err)
	}
}

func TestHTTPReceiptSenderRetainsBoundedNonSuccessStatus(t *testing.T) {
	closeCause := errors.New("SECRET rejected response close")
	transport := receiptRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: receiptResponseBody{reader: bytes.NewReader([]byte("private response")), closeErr: closeCause}}, nil
	})
	sender, err := NewHTTPReceiptSender("https://email.example.test/send", "test-token", "receipts@example.invalid", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.SendReceipt(context.Background(), ReceiptMessage{IdempotencyKey: "tir_1", To: "recipient@example.invalid", Subject: "Files received", Text: "Paperboat received 2 files."})
	var providerStatus interface{ DiagnosticStatus() int }
	if !errors.As(err, &providerStatus) || providerStatus.DiagnosticStatus() != http.StatusTooManyRequests || !errors.Is(err, closeCause) || err.Error() != "receipt email delivery failed" {
		t.Fatalf("bounded provider status or private close cause was lost: %v", err)
	}
}

type readFailure struct{ err error }

func (r readFailure) Read([]byte) (int, error) { return 0, r.err }

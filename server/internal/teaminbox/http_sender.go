package teaminbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type HTTPReceiptSender struct {
	endpoint, token, from string
	client                *http.Client
}

type receiptDeliveryError struct{ cause error }

func (receiptDeliveryError) Error() string   { return "receipt email delivery failed" }
func (e receiptDeliveryError) Unwrap() error { return e.cause }

type receiptHTTPStatusError struct{ status int }

func (receiptHTTPStatusError) Error() string { return "receipt email provider rejected delivery" }
func (e receiptHTTPStatusError) DiagnosticStatus() int {
	if e.status < 100 || e.status > 599 {
		return 0
	}
	return e.status
}

type receiptResponseError struct{ cause error }

func (receiptResponseError) Error() string   { return "receipt email response is invalid" }
func (e receiptResponseError) Unwrap() error { return e.cause }

func NewHTTPReceiptSender(endpoint, token, from string, client *http.Client) (*HTTPReceiptSender, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) || strings.TrimSpace(token) == "" || strings.TrimSpace(from) == "" || strings.ContainsAny(from, "\r\n\x00") {
		return nil, ErrInvalid
	}
	if client == nil {
		client = &http.Client{}
	}
	return &HTTPReceiptSender{endpoint: u.String(), token: token, from: from, client: client}, nil
}

func (s *HTTPReceiptSender) SendReceipt(ctx context.Context, message ReceiptMessage) (string, error) {
	if message.IdempotencyKey == "" || message.To == "" || strings.ContainsAny(message.To, "\r\n\x00") || len(message.Subject) > 200 || len(message.Text) > 2048 {
		return "", ErrInvalid
	}
	payload, _ := json.Marshal(map[string]string{"from": s.from, "to": message.To, "subject": message.Subject, "text": message.Text})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", receiptDeliveryError{cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", message.IdempotencyKey)
	response, err := s.client.Do(request)
	if err != nil {
		return "", receiptDeliveryError{cause: err}
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	closeErr := response.Body.Close()
	if readErr != nil {
		cause := error(readErr)
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			cause = errors.Join(receiptHTTPStatusError{status: response.StatusCode}, cause)
		}
		return "", receiptDeliveryError{cause: errors.Join(cause, closeErr)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", receiptDeliveryError{cause: errors.Join(receiptHTTPStatusError{status: response.StatusCode}, closeErr)}
	}
	if len(body) > 4096 {
		return "", receiptResponseError{cause: closeErr}
	}
	var result struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", receiptResponseError{cause: errors.Join(err, closeErr)}
	}
	if strings.TrimSpace(result.MessageID) == "" || len(result.MessageID) > 256 || strings.ContainsAny(result.MessageID, "\r\n\x00") {
		return "", receiptResponseError{cause: closeErr}
	}
	if closeErr != nil {
		// The provider already confirmed the message ID. Retain that receipt so
		// the retry owner does not send a duplicate when only body cleanup failed.
		return result.MessageID, receiptDeliveryError{cause: closeErr}
	}
	return result.MessageID, nil
}

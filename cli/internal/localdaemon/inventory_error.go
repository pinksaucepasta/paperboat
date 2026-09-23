package localdaemon

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"net"
	"strconv"
)

type inventorySourceError struct {
	stage string
	cause error
}

func (e *inventorySourceError) Error() string { return "machine inventory failed at " + e.stage }
func (e *inventorySourceError) Unwrap() error { return e.cause }
func inventorySourceFailure(stage string, err error) error {
	var existing *inventorySourceError
	if errors.As(err, &existing) {
		return err
	}
	return &inventorySourceError{stage: stage, cause: err}
}

// Diagnostics contain only locally selected stages/categories and HTTP status.
// API messages, codes, URLs, credentials and response bodies are never recorded.
func inventoryRefreshDiagnostic(err error) (string, map[string]string) {
	fields := map[string]string{"outcome": "ready"}
	if err == nil {
		return "info", fields
	}
	fields["outcome"] = "degraded"
	fields["phase"] = "inventory"
	var source *inventorySourceError
	if errors.As(err, &source) {
		fields["phase"] = source.stage
	}
	reason := "source_error"
	var response *api.APIError
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		reason = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "deadline"
	case errors.Is(err, api.ErrUnauthenticated):
		reason = "unauthenticated"
	case errors.As(err, &response):
		if response.Status >= 100 && response.Status <= 599 {
			reason = "http_" + strconv.Itoa(response.Status)
		} else {
			reason = "http_invalid_status"
		}
	case errors.As(err, &network):
		reason = "network"
		if network.Timeout() {
			reason = "network_timeout"
		}
	}
	fields["reason"] = reason
	return "warning", fields
}

package tunnelenrollment

import (
	"strconv"
	"time"
)

const (
	carrierBootstrapSchema      = "paperboat.connector-bootstrap/v1"
	carrierBootstrapErrorSchema = "paperboat.preview-tunnel/v1"
	controlSubprotocol          = "paperboat.connector.v1"
	bootstrapResponseLimit      = 16 << 20
	controlDialTimeout          = 15 * time.Second
)

// CarrierBootstrapError preserves the server's finite, machine-readable
// failure contract. Callers must branch on Code, Retryable, and RepairAction,
// rather than parsing a server message. StatusCode is retained for diagnostics
// and fallback handling when a deployment returns a valid error envelope with
// an unexpected status.
type CarrierBootstrapError struct {
	StatusCode       int
	Code             string
	Retryable        bool
	RetryAt          *time.Time
	RepairAction     string
	RequestID        string
	CorrelationID    string
	SupportReference string
}

func (e *CarrierBootstrapError) Error() string {
	if e == nil {
		return "carrier bootstrap failed"
	}
	if e.Code != "" {
		return "carrier bootstrap failed: " + e.Code
	}
	if e.StatusCode != 0 {
		return "carrier bootstrap failed: HTTP " + strconv.Itoa(e.StatusCode)
	}
	return "carrier bootstrap failed"
}

// ControlIdentity is the safe, server-authoritative identity returned by the
// enrollment exchange plus the registered account binding held by hostd.
type ControlIdentity struct {
	AccountID            string
	ProcessGeneration    uint64
	CredentialGeneration uint64
}

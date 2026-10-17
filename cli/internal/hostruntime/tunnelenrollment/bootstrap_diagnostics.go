package tunnelenrollment

import (
	"errors"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

// Preserve only the finite bootstrap contract and non-secret binding tuple.
// Server messages, authentication headers, proofs and response bytes are excluded.
func (s *HTTPSProductionAssemblySource) recordCarrierBootstrapFailure(tuple carrierBootstrapRequest, err error) {
	var failure *CarrierBootstrapError
	if !errors.As(err, &failure) || !knownCarrierBootstrapErrorCode(failure.Code) {
		return
	}
	s.mu.Lock()
	recorder := s.recorder
	s.mu.Unlock()
	if recorder == nil {
		return
	}
	reference := failure.SupportReference
	if !supportref.Valid(reference) {
		reference = ""
	}
	retry, action := "terminal", "stop"
	if failure.Retryable {
		retry, action = "retryable", "retry"
	}
	if failure.RepairAction == "reconnect" {
		action = "reconnect"
	}
	_ = recorder.RecordWithSupportReference("tunnel", failure.Code, "warning", reference, map[string]string{
		"operation": "carrier_bootstrap", "http_status": strconv.Itoa(failure.StatusCode), "reason": action, "retry_class": retry,
		"session_id": tuple.SessionID, "process_generation": strconv.FormatUint(tuple.ProcessGeneration, 10),
		"config_generation": strconv.FormatUint(tuple.ConfigGeneration, 10), "config_content_hash": tuple.ConfigContentHash,
	})
}

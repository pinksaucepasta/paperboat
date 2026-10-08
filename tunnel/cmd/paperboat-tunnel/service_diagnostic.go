package main

import (
	"fmt"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/reporting"
)

func serviceDiagnostic(err error) string {
	cause, errorType, httpStatus := reporting.SafeFailureDetails(err)
	detail := fmt.Sprintf("cause=%s error_type=%s", cause, errorType)
	if httpStatus != 0 {
		detail += fmt.Sprintf(" http_status=%d", httpStatus)
	}
	return detail
}

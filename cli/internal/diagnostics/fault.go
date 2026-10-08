package diagnostics

import (
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

// RecordFault stores the already redacted fault projection. It only enqueues;
// persistence and its independent failure/drop counters belong to DiskRing.
func (r *Recorder) RecordFault(fault errorreport.Fault) error {
	fields := map[string]string{
		"component": fault.Component, "operation": fault.Operation,
		"outcome": fault.Outcome, "cause": fault.Cause,
		"error_type": fault.ErrorType,
	}
	if len(fault.ErrorChain) > 0 {
		fields["error_chain"] = strings.Join(fault.ErrorChain, ":")
	}
	if fault.ServiceComponent != "" {
		fields["service_component"] = fault.ServiceComponent
	}
	if fault.Errno != 0 {
		fields["errno"] = strconv.Itoa(fault.Errno)
	}
	if fault.HTTPStatus != 0 {
		fields["http_status"] = strconv.Itoa(fault.HTTPStatus)
	}
	if fault.SourceFile != "" {
		fields["source_file"] = fault.SourceFile
	}
	if fault.SourceFunction != "" {
		fields["source_function"] = fault.SourceFunction
	}
	if fault.SourceLine > 0 {
		fields["source_line"] = strconv.Itoa(fault.SourceLine)
	}
	return r.RecordWithSupportReference(fault.Stage, fault.Code, fault.Severity, fault.SupportReference, fields)
}

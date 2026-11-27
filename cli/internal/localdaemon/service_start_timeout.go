package localdaemon

import (
	"context"

	"github.com/pinksaucepasta/paperboat/cli/internal/diagnostics"
)

// ServiceStartTimeoutCode is the diagnostics code recorded when Windows reports
// that the local daemon service did not answer its start request in time.
const ServiceStartTimeoutCode = "local_daemon_service_start_timeout"

func recordServiceStartTimeout(ctx context.Context, service string) {
	if recorder := diagnostics.FromContext(ctx); recorder != nil {
		_ = recorder.Record("local_daemon", ServiceStartTimeoutCode, "warning", map[string]string{"service": service})
	}
}

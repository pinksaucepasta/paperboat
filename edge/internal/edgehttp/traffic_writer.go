package edgehttp

import (
	"io"

	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
)

// trafficWriter counts the bytes written through it as node traffic ("ingress": from a visitor toward the machine,
// "egress": back), for bridges that copy raw connections and so never pass the HTTP request telemetry.
type trafficWriter struct {
	io.Writer
	direction string
}

func (w trafficWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		edgetelemetry.RecordTraffic(w.direction, uint64(n))
	}
	return n, err
}

package connectorprotocol

// OriginFailureHeader carries why a host could not reach a route's origin to the edge, which shows
// the matching page. The host sets it after stripping every X-Paperboat-* header the origin sent,
// so the edge can trust it. Its values are the OriginFailure codes below.
const OriginFailureHeader = "X-Paperboat-Origin-Failure"

// Origin-failure codes: the host's origin-health result, also reported to the control plane.
const (
	OriginRefused            = "refused"
	OriginTimeout            = "timeout"
	OriginTLS                = "tls"
	OriginCertificateChanged = "certificate_changed"
	OriginUnreachable        = "unreachable"
)

// OriginFailureCodes lists every code a host can send, so the edge's page table can be checked
// against the producer.
var OriginFailureCodes = []string{OriginRefused, OriginTimeout, OriginTLS, OriginCertificateChanged, OriginUnreachable}

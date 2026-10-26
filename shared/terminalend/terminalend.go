// Package terminalend names why a browser terminal connection was refused or ended. The server states a reason on the
// edge control channel; the edge closes the browser's WebSocket with the matching code and the same reason, so the
// console can say what happened instead of reconnecting blindly. One definition for the server, the edge and the
// browser worker.
package terminalend

// Reason is the close reason on the wire.
type Reason string

const (
	// NoAccess: the terminal does not exist or is not shared with this browser (or its access changed).
	NoAccess Reason = "no_access"
	// Closed: the owner's terminal has closed or exited.
	Closed Reason = "closed"
	// Revoked: access ended while attached (sign-out, machine revoked, code rotated or expired).
	Revoked Reason = "revoked"
	// Ended: the terminal's session ended on the machine.
	Ended Reason = "ended"
	// HostUnreachable: the machine did not answer through its edge connection.
	HostUnreachable Reason = "host_unreachable"
	// EdgeUnavailable: the machine has no connected edge route, or the edge or control plane cannot admit now.
	EdgeUnavailable Reason = "edge_unavailable"
	// BandwidthQuotaExhausted: the paying workspace's hosted relay and edge allowance is used up.
	BandwidthQuotaExhausted Reason = "bandwidth_quota_exhausted"
	// TicketInvalid: the one-use ticket lapsed or was already used; a fresh ticket retries.
	TicketInvalid Reason = "ticket_invalid"
	// Capacity: too many attachments for this browser or this edge right now; retry later.
	Capacity Reason = "capacity"
)

// WebSocket close codes. 4xxx are private; 1013 is the standard "try again later".
const (
	CodeNoAccess    = 4404
	CodeEnded       = 4409
	CodeUnavailable = 4503
	CodeQuota       = 4429
	CodeRetry       = 1013
)

// CloseCode is the code the browser's WebSocket is closed with for the reason.
func (r Reason) CloseCode() int {
	switch r {
	case NoAccess:
		return CodeNoAccess
	case Closed, Revoked, Ended:
		return CodeEnded
	case HostUnreachable, EdgeUnavailable:
		return CodeUnavailable
	case BandwidthQuotaExhausted:
		return CodeQuota
	default:
		return CodeRetry
	}
}

// Parse returns the reason a control channel close names; an unknown or empty one is TicketInvalid, which the
// console treats as a retry with a fresh ticket.
func Parse(value string) Reason {
	switch r := Reason(value); r {
	case NoAccess, Closed, Revoked, Ended, HostUnreachable, EdgeUnavailable, BandwidthQuotaExhausted, TicketInvalid, Capacity:
		return r
	}
	return TicketInvalid
}

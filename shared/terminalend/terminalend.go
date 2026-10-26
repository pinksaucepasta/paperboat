// Package terminalend names why a browser terminal connection was refused or ended. The server states a reason on the
// edge control channel; the edge closes the browser's WebSocket with the matching code and the same reason, so the
// console can say what happened instead of reconnecting blindly. One definition for the server, the edge and the
// browser worker.
package terminalend

import "strings"

// Reason is the typed code a close carries on the wire (a close reason is the code, optionally followed by
// ": <human text>"). The browser worker maps it and the console branches on it alone; the WebSocket close code is only
// a coarse class for tools.
type Reason string

const (
	// NoAccess: the terminal does not exist, is not shared with this browser, or its access changed.
	NoAccess Reason = "no_access"
	// Ended: the owner's terminal has closed or exited, or its session ended on the machine.
	Ended Reason = "ended"
	// Revoked: access ended while attached (sign-out, machine revoked, code rotated or expired).
	Revoked Reason = "revoked"
	// Expired: the one-use ticket lapsed or was already used; a fresh ticket retries without an outage message.
	Expired Reason = "expired"
	// Limit: the plan's open browser terminals limit is reached.
	Limit Reason = "limit"
	// BandwidthQuotaExhausted: the paying workspace's hosted relay and edge allowance is used up. The console
	// already matches this exact string.
	BandwidthQuotaExhausted Reason = "bandwidth_quota_exhausted"
	// TooManyPending: too many tickets or attachments for this browser or edge right now; retry after a wait.
	TooManyPending Reason = "too_many_pending"
	// MachineUnavailable: the machine did not answer through its edge connection, or has no route.
	MachineUnavailable Reason = "machine_unavailable"
	// EdgeUnavailable: the edge or the control plane cannot admit now; retry shortly.
	EdgeUnavailable Reason = "edge_unavailable"
	// Protocol: a message or framing error.
	Protocol Reason = "protocol"
)

// WebSocket close codes (private 4xxx): one per reason, so a tool without the table still sees the class.
const (
	CodeRevoked            = 4401
	CodeLimit              = 4402
	CodeQuota              = 4403
	CodeNoAccess           = 4404
	CodeExpired            = 4408
	CodeEnded              = 4409
	CodeProtocol           = 4500
	CodeMachineUnavailable = 4503
	CodeEdgeUnavailable    = 4504
	CodeTooManyPending     = 4429
)

// CloseCode is the WebSocket close code for the reason.
func (r Reason) CloseCode() int {
	switch r {
	case NoAccess:
		return CodeNoAccess
	case Ended:
		return CodeEnded
	case Revoked:
		return CodeRevoked
	case Expired:
		return CodeExpired
	case Limit:
		return CodeLimit
	case BandwidthQuotaExhausted:
		return CodeQuota
	case TooManyPending:
		return CodeTooManyPending
	case MachineUnavailable:
		return CodeMachineUnavailable
	case EdgeUnavailable:
		return CodeEdgeUnavailable
	}
	return CodeProtocol
}

// Parse returns the reason a close names. The text after ": " is ignored; an unknown or empty reason is Protocol.
func Parse(value string) Reason {
	code, _, _ := strings.Cut(value, ": ")
	switch r := Reason(code); r {
	case NoAccess, Ended, Revoked, Expired, Limit, BandwidthQuotaExhausted, TooManyPending, MachineUnavailable, EdgeUnavailable, Protocol:
		return r
	}
	return Protocol
}

// ForCloseCode is the reason for a WebSocket close code that arrived without a reason; an unknown code is Protocol.
func ForCloseCode(code int) Reason {
	for _, r := range []Reason{NoAccess, Ended, Revoked, Expired, Limit, BandwidthQuotaExhausted, TooManyPending, MachineUnavailable, EdgeUnavailable} {
		if r.CloseCode() == code {
			return r
		}
	}
	return Protocol
}

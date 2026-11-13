package datacarrier

import "errors"

// CarrierRefusal names why a carrier's client certificate matched no admission,
// so a refused handshake logs its cause instead of a bare TLS failure. Larger
// values are more specific; AnyPeerBinding reports the most specific one.
type CarrierRefusal int

const (
	// RefusalNoAdmissions: this process holds no admission at all yet.
	RefusalNoAdmissions CarrierRefusal = iota + 1
	// RefusalUnknownMachine: no admission names this machine key.
	RefusalUnknownMachine
	// RefusalNoLiveAdmission: the machine's admissions are expired or not yet admitted.
	RefusalNoLiveAdmission
	// RefusalStaleIdentity: the certificate names another session, generation or edge process epoch.
	RefusalStaleIdentity
)

func (r CarrierRefusal) Error() string { return "carrier refused: " + r.Code() }

// Code is the refusal's fixed log value.
func (r CarrierRefusal) Code() string {
	switch r {
	case RefusalNoAdmissions:
		return "no_admissions"
	case RefusalUnknownMachine:
		return "unknown_machine"
	case RefusalNoLiveAdmission:
		return "no_live_admission"
	case RefusalStaleIdentity:
		return "stale_identity"
	}
	return "refused"
}

// FailureReason names a carrier failure with a fixed log value: the refusal reason, tls_handshake
// for any other failed handshake, otherwise carrier.
func FailureReason(err error) string {
	var reason CarrierRefusal
	if errors.As(err, &reason) {
		return reason.Code()
	}
	if errors.Is(err, errCarrierTLSHandshake) {
		return "tls_handshake"
	}
	return "carrier"
}

// refusal classifies a miss from what matched: any admission at all, one for
// this machine key, and one of those live.
func refusal(any, keyKnown, live bool) CarrierRefusal {
	switch {
	case !any:
		return RefusalNoAdmissions
	case !keyKnown:
		return RefusalUnknownMachine
	case !live:
		return RefusalNoLiveAdmission
	}
	return RefusalStaleIdentity
}

package datacarrier

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

func (r CarrierRefusal) Error() string {
	switch r {
	case RefusalNoAdmissions:
		return "carrier refused: no_admissions"
	case RefusalUnknownMachine:
		return "carrier refused: unknown_machine"
	case RefusalNoLiveAdmission:
		return "carrier refused: no_live_admission"
	case RefusalStaleIdentity:
		return "carrier refused: stale_identity"
	}
	return "carrier refused"
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

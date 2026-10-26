package terminalend

import "testing"

func TestEveryReasonHasOneCloseCodeAndParsesBack(t *testing.T) {
	want := map[Reason]int{
		NoAccess: CodeNoAccess, Closed: CodeEnded, Revoked: CodeEnded, Ended: CodeEnded,
		HostUnreachable: CodeUnavailable, EdgeUnavailable: CodeUnavailable,
		BandwidthQuotaExhausted: CodeQuota, TicketInvalid: CodeRetry, Capacity: CodeRetry,
	}
	for reason, code := range want {
		if got := reason.CloseCode(); got != code {
			t.Errorf("%s closes with %d, want %d", reason, got, code)
		}
		if Parse(string(reason)) != reason {
			t.Errorf("%s does not parse back", reason)
		}
	}
}

func TestAnUnknownReasonIsARetry(t *testing.T) {
	for _, value := range []string{"", "admission denied", "access revoked", "surprise"} {
		if got := Parse(value); got != TicketInvalid || got.CloseCode() != CodeRetry {
			t.Errorf("Parse(%q) = %s/%d, want ticket_invalid retry", value, got, got.CloseCode())
		}
	}
}

// The browser worker and the console match these strings; changing one is a contract change.
func TestWireValuesAreStable(t *testing.T) {
	if BandwidthQuotaExhausted != "bandwidth_quota_exhausted" || CodeNoAccess != 4404 || CodeEnded != 4409 || CodeUnavailable != 4503 || CodeQuota != 4429 {
		t.Fatal("a browser terminal close value changed")
	}
}

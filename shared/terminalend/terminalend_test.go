package terminalend

import "testing"

func TestEveryReasonHasItsOwnCloseCodeAndParsesBack(t *testing.T) {
	want := map[Reason]int{
		NoAccess: 4404, Ended: 4409, Revoked: 4401, Expired: 4408, Limit: 4402, BandwidthQuotaExhausted: 4403,
		TooManyPending: 4429, MachineUnavailable: 4503, EdgeUnavailable: 4504, Protocol: 4500,
	}
	seen := map[int]Reason{}
	for reason, code := range want {
		if got := reason.CloseCode(); got != code {
			t.Errorf("%s closes with %d, want %d", reason, got, code)
		}
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s share close code %d", reason, other, code)
		}
		seen[code] = reason
		if Parse(string(reason)) != reason || Parse(string(reason)+": any human text") != reason {
			t.Errorf("%s does not parse back, with or without text", reason)
		}
	}
}

func TestAnUnknownReasonIsAProtocolError(t *testing.T) {
	for _, value := range []string{"", "admission denied", "access revoked", "ws_4503", "no_access:missing-space"} {
		if got := Parse(value); got != Protocol || got.CloseCode() != CodeProtocol {
			t.Errorf("Parse(%q) = %s/%d, want protocol", value, got, got.CloseCode())
		}
	}
}

// The browser worker and the console match these strings; changing one is a contract change.
func TestWireValuesAreStable(t *testing.T) {
	if BandwidthQuotaExhausted != "bandwidth_quota_exhausted" || NoAccess != "no_access" || MachineUnavailable != "machine_unavailable" {
		t.Fatal("a browser terminal close reason changed")
	}
}

func TestForCloseCodeInvertsCloseCode(t *testing.T) {
	for _, r := range []Reason{NoAccess, Ended, Revoked, Expired, Limit, BandwidthQuotaExhausted, TooManyPending, MachineUnavailable, EdgeUnavailable, Protocol} {
		if got := ForCloseCode(r.CloseCode()); got != r {
			t.Fatalf("ForCloseCode(%d) = %s, want %s", r.CloseCode(), got, r)
		}
	}
	if got := ForCloseCode(4999); got != Protocol {
		t.Fatalf("unknown code = %s, want protocol", got)
	}
}

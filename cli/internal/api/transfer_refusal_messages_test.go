package api

import "testing"

// PLAN 3b File transfer modes step 1 (Lane O): the server's 409 refusals render from their details.
func TestTransferRefusalsRenderFromTheirDetails(t *testing.T) {
	large := apiErrorMessageFor("transfer_too_large", 409, map[string]any{"total_bytes": float64(3_400_000_000), "max_bytes": float64(2_000_000_000)})
	if large != "This transfer is 3.4 GB. Automatic sends up to 2 GB. Send less than 2 GB." {
		t.Fatalf("transfer_too_large = %q", large)
	}
}

// Lane O 44a17b1f: a team at its cap answers 409 team_limit; the CLI said "resource state changed".
func TestTeamLimitRendersFromItsDetails(t *testing.T) {
	// XE item 9: every kind names its limit and the numbers, not only machines.
	for kind, want := range map[string]string{
		"machine":          "Team limit reached: 10000 of 10000 machines. Remove one the team no longer uses, then retry.",
		"tunnel":           "Team limit reached: 10000 of 10000 previews and tunnels. Remove one the team no longer uses, then retry.",
		"terminal_session": "Team limit reached: 1000 of 1000 shared terminal sessions. Remove one the team no longer uses, then retry.",
		"member":           "Team limit reached: 128 of 128 members. Remove one the team no longer uses, then retry.",
	} {
		limit := map[string]float64{"terminal_session": 1000, "member": 128}[kind]
		if limit == 0 {
			limit = 10000
		}
		if got := apiErrorMessageFor("team_limit", 409, map[string]any{"kind": kind, "used": limit, "limit": limit}); got != want {
			t.Errorf("%s team_limit = %q, want %q", kind, got, want)
		}
	}
	if got := apiErrorMessageFor("team_limit", 409, nil); got != "Team capacity reached. Remove an entry the team no longer uses, then retry." {
		t.Fatalf("generic team_limit = %q", got)
	}
}

// The refusal reaches the user through APIError.Error(); it used to print the bare 409 text.
func TestAPIErrorTextCarriesTheRefusalDetails(t *testing.T) {
	err := &APIError{Status: 409, Code: "transfer_too_large", Details: map[string]any{"total_bytes": float64(3_400_000_000), "max_bytes": float64(2_000_000_000)}}
	if got := err.Error(); got != "This transfer is 3.4 GB. Automatic sends up to 2 GB. Send less than 2 GB." {
		t.Fatalf("APIError.Error() = %q", got)
	}
}

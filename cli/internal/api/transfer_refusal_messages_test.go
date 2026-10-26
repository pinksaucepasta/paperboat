package api

import "testing"

// PLAN 3b File transfer modes step 1 (Lane O): the server's 409 refusals render from their details.
func TestTransferRefusalsRenderFromTheirDetails(t *testing.T) {
	update := apiErrorMessageFor("destination_update_required", 409, map[string]any{"machine": "hp", "max_bytes": float64(524288000), "minimum_version": "2026.10.27.0"})
	if update != "hp needs pb 2026.10.27.0 or later to receive files over 500 MB (or unlimited). Update pb on hp (pb update), or send less than 500 MB." {
		t.Fatalf("destination_update_required = %q", update)
	}
	large := apiErrorMessageFor("transfer_too_large", 409, map[string]any{"total_bytes": float64(3_400_000_000), "max_bytes": float64(2_000_000_000)})
	if large != "This transfer is 3.4 GB. Automatic sends up to 2 GB. Send less than 2 GB." {
		t.Fatalf("transfer_too_large = %q", large)
	}
	if forged := apiErrorMessageFor("destination_update_required", 409, map[string]any{"machine": "hp; rm -rf /", "max_bytes": float64(1), "minimum_version": "x"}); forged == "" || forged[:2] == "hp" {
		t.Fatalf("free text from details was rendered: %q", forged)
	}
}

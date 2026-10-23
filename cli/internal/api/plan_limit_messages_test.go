package api

import (
	"net/http"
	"strings"
	"testing"
)

// The plan-limit refusals (Lane O, server ce8211a7) name the limit, the way out and the Billing link,
// and keep their codes for --json.
func TestPlanLimitRefusalsNameTheLimitAndTheWayOut(t *testing.T) {
	details := map[string]any{"kind": "machines", "plan": "free", "limit": float64(100), "used": float64(100), "billing_url": "https://console.example.test/billing"}
	machines := apiErrorMessageFor("machine_limit_reached", http.StatusConflict, details)
	if !strings.Contains(machines, "allows 100 machines") || !strings.Contains(machines, "pb machine revoke") || !strings.HasSuffix(machines, ": https://console.example.test/billing") {
		t.Fatalf("machines: %q", machines)
	}
	previews := apiErrorMessageFor("preview_limit_reached", http.StatusConflict, details)
	if !strings.Contains(previews, "allows 100 running previews and tunnels") || !strings.Contains(previews, "Stop one") || !strings.HasSuffix(previews, "https://console.example.test/billing") {
		t.Fatalf("previews: %q", previews)
	}
	if bare := apiErrorMessageFor("machine_limit_reached", http.StatusConflict, nil); bare != publicAPIMessages["machine_limit_reached"] {
		t.Fatalf("without details: %q", bare)
	}
	for _, code := range []string{"machine_limit_reached", "preview_limit_reached"} {
		if (&APIError{Code: code}).PublicCode() != code {
			t.Fatalf("%s is not a public code", code)
		}
	}
}

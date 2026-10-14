package httpapi

import "testing"

func TestWorkspaceBillingReturnOrigin(t *testing.T) {
	origins := []string{"https://dashboard.example.test", "http://100.64.0.1:3000"}
	for _, raw := range []string{"https://dashboard.example.test/dashboard/subscriptions", "http://100.64.0.1:3000/dashboard/teams?paid=1"} {
		if !billingReturnAllowed(raw, origins) {
			t.Errorf("valid dashboard return denied: %s", raw)
		}
	}
	for _, raw := range []string{"https://dashboard.example.test.evil.test/", "https://dashboard.example.test@evil.test/", "https://user@dashboard.example.test/", "https://dashboard.example.test:444/", "//dashboard.example.test/", "javascript:alert(1)", "https://dashboard.example.test/#secret"} {
		if billingReturnAllowed(raw, origins) {
			t.Errorf("unsafe return accepted: %s", raw)
		}
	}
}

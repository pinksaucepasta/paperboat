package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/auth"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
)

func TestTopupHTTPRequiresIntegerQuantity(t *testing.T) {
	s := billing.NewWorkspaceService(nil, nil, "")
	handler := workspaceBillingMutation(s, []string{"https://dashboard.example.test"}, false)
	for _, tc := range []struct {
		name, quantity string
		want           int
	}{
		{"missing", "", 400}, {"zero", `,"quantity":0`, 400}, {"negative", `,"quantity":-1`, 400}, {"fraction", `,"quantity":1.5`, 400}, {"above-max", `,"quantity":101`, 400}, {"integer", `,"quantity":3`, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"product_code":"topup","product_version":1,"success_url":"https://dashboard.example.test/billing?done=1","cancel_url":"https://dashboard.example.test/billing"` + tc.quantity + `}`
			r := httptest.NewRequest(http.MethodPost, "/v1/billing/workspaces/personal/topups", strings.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_fixture"}}))
			r.Header.Set("Idempotency-Key", "op_fixture")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("quantity HTTP status=%d want=%d", w.Code, tc.want)
			}
		})
	}
}

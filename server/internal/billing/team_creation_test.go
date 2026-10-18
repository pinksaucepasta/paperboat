package billing_test

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/billing"
	"testing"
)

func TestPaidTeamCreationRejectsInvalidPurchase(t *testing.T) {
	valid := billing.TeamCreationInput{OperationID: "operation-1", TeamID: "my-team", Seats: 1, Interval: "month", SuccessURL: "https://console.example.test/success", CancelURL: "https://console.example.test/cancel"}
	for _, tc := range []struct {
		name   string
		change func(*billing.TeamCreationInput)
	}{
		{"free request", func(in *billing.TeamCreationInput) { in.Seats = 0 }},
		{"negative seats", func(in *billing.TeamCreationInput) { in.Seats = -1 }},
		{"seat bound", func(in *billing.TeamCreationInput) { in.Seats = 129 }},
		{"reserved personal", func(in *billing.TeamCreationInput) { in.TeamID = "personal" }},
		{"invalid slug", func(in *billing.TeamCreationInput) { in.TeamID = "Upper Case" }},
		{"missing operation", func(in *billing.TeamCreationInput) { in.OperationID = "" }},
		{"invalid interval", func(in *billing.TeamCreationInput) { in.Interval = "monthly" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.change(&in)
			s := billing.NewWorkspaceService(nil, nil, "")
			if _, err := s.CreatePaidTeam(context.Background(), "actor", "", in); !errors.Is(err, billing.ErrTeamCreationInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	for _, interval := range []string{"month", "year"} {
		in := valid
		in.Interval = interval
		if _, err := billing.NewWorkspaceService(nil, nil, "").CreatePaidTeam(context.Background(), "actor", "", in); !errors.Is(err, billing.ErrStripeUnavailable) {
			t.Fatalf("valid %s interval rejected: %v", interval, err)
		}
	}

}

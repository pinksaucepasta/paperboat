package teams

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestInvitationMailUsesTheSharedLayout(t *testing.T) {
	m := invitationMessage(mailtest.Policy, "member@example.test", "Acme <Team>", time.Date(2026, 12, 19, 10, 0, 0, 0, time.UTC))
	mailtest.Check(t, "team-invitation", m, "https://console.example.test/team", "expires 19 December 2026, 10:00 UTC.")
}

package auditstream

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestFailingMailUsesTheSharedLayout(t *testing.T) {
	link := "https://console.example.test/team?team=team_1&tab=activity"
	m := failingMessage(mailtest.Policy, "admin@example.test", "Acme", "HTTPS endpoint", "destination answered 503", link, time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC))
	mailtest.Check(t, "audit-stream-failing", m, link)
}

package operatoractions

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestBanAndBlockMailsUseTheSharedLayout(t *testing.T) {
	mailtest.Check(t, "account-banned", bannedMessage(mailtest.Policy, "person@example.test", "Your account", "spam <b>relay</b>", "ban_123"), "")
	mailtest.Check(t, "publication-disabled", blockedMessage(mailtest.Policy, "person@example.test", "app.example.test", "phishing"), "")
}

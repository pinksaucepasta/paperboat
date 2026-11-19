package accountdeletion

import (
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestDeletedMailUsesTheSharedLayout(t *testing.T) {
	mailtest.Check(t, "account-deleted", deletedMessage(mailtest.Policy, "person@example.test"), "")
}

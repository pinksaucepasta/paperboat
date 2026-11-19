package billing

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail/mailtest"
)

func TestThrottledMailUsesTheSharedLayout(t *testing.T) {
	url := "https://console.example.test/billing"
	m := throttledMessage(mailtest.Policy, "owner@example.test", "Acme", "1 Mbit/s", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), url)
	mailtest.Check(t, "bandwidth-throttled", m, url, "returns on 1 December 2026,")
}

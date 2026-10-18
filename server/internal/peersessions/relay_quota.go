package peersessions

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/bandwidthgate"
)

// RelayDeniedQuotaExhausted is the typed reason a network refresh carries when hosted relay
// grants were withheld because the account has no bandwidth allowance left anywhere.
const RelayDeniedQuotaExhausted = "bandwidth_quota_exhausted"

// ConfigureQuota makes hosted relay grants admission-checked against the account's remaining
// bandwidth (internal/bandwidthgate: read-only, cached, fail-open). billingURL is the console's
// Billing page, published to the client; it may be empty.
func (s *NetworkService) ConfigureQuota(billingURL string) {
	s.quotaGate, s.billingURL = bandwidthgate.New(s.store), billingURL
}

// relayQuotaWithheld is the network-configuration gate on a hot path; see bandwidthgate for why
// it can neither block nor fail the configuration.
func (s *NetworkService) relayQuotaWithheld(ctx context.Context, account string, at time.Time) bool {
	return s.quotaGate.Denied(ctx, account, at)
}

package localdaemon

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// relayDenialSource adds the hosted relay plan limit to the daemon snapshot while the latest network
// refresh withheld relays for it. A stream the relay cut at the limit ends like any lost connection;
// this lets pb status and the failed command name the limit and the Billing action instead.
// It embeds the inventory so the local API still finds its refresh and watch methods.
type relayDenialSource struct {
	*Inventory
	denial func() *api.APIError
}

func (s relayDenialSource) Snapshot(ctx context.Context) (localapi.Snapshot, error) {
	snapshot, err := s.Inventory.Snapshot(ctx)
	if err != nil || s.denial == nil {
		return snapshot, err
	}
	if denial := s.denial(); denial != nil && denial.Code == localapi.RelayQuotaCode {
		snapshot.Health = append(snapshot.Health, localapi.HealthItem{Code: localapi.RelayQuotaCode, Severity: "warning", Title: denial.Message, Recovery: RelayQuotaRecovery, ETag: localapi.RelayQuotaCode})
	}
	return snapshot, nil
}

// RelayQuotaRecovery is the action for an exhausted hosted relay allowance.
const RelayQuotaRecovery = "Add bandwidth or upgrade under Billing in the Paperboat console, or wait for the next allowance period. Direct connections keep working."

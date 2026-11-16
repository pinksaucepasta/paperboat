package tailnet

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// persistedRelays is the relay list and the relay grants of the last answered renewal, kept with the configuration so
// a daemon restarted while Paperboat is unreachable opens relayed paths through its last known relays (behind NAT a
// restart otherwise reached no peer until Paperboat answered: winpool runner, 2026-11-16). Both last at most
// relaylimits.MaxGrantLifetime and each relay enforces its grant; on restore every claim is checked against the
// restored configuration (generation, binding, peers), so a stale file applies nothing. Prior art: Tailscale keeps its
// DERP map in the cached netmap it loads before contacting control (ipn/ipnlocal/local.go:2118-2126).
type persistedRelays struct {
	CandidateSet string   `json:"candidate_set"`
	RelayGrants  []string `json:"relay_grants"`
}

// maxPersistedRelaysBytes bounds the file: one candidate set plus every grant part.
const maxPersistedRelaysBytes = 8 << 20

func (a *Authority) saveRelays(candidateSet string, grants []string) {
	data, err := json.Marshal(persistedRelays{CandidateSet: candidateSet, RelayGrants: grants})
	digest := sha256.Sum256(data)
	if err == nil && digest == a.savedRelays {
		return
	}
	if err == nil && len(data) <= maxPersistedRelaysBytes {
		err = a.options.Store.SavePeerNetworkRelays(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, data)
	}
	if err != nil {
		diagnosticlog.TryWarn("relay list not saved for a restart", "error", err)
		return
	}
	a.savedRelays = digest
}

// restoreRelays applies the persisted relay state to the configuration Restore just installed. A failure leaves only
// direct paths until the first renewal, as before.
func (a *Authority) restoreRelays(ctx context.Context) {
	data, err := a.options.Store.LoadPeerNetworkRelays(a.options.Issuer, a.options.Self.AccountID, a.options.Self.EndpointID, maxPersistedRelaysBytes)
	var saved persistedRelays
	if err != nil || data == nil || json.Unmarshal(data, &saved) != nil || saved.CandidateSet == "" {
		return
	}
	if err := a.applyRegionalCandidates(ctx, saved.CandidateSet, true); err != nil {
		diagnosticlog.TryInfo("native packet lifecycle", "stage", "restore_relays_refused", "error", err)
		return
	}
	if err := a.applyRelayGrants(ctx, saved.RelayGrants, true); err != nil {
		diagnosticlog.TryInfo("native packet lifecycle", "stage", "restore_relays_refused", "error", err)
		return
	}
	diagnosticlog.TryInfo("native packet lifecycle", "stage", "restore_relays_ok")
}

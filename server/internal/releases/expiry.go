package releases

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ExpiryWarnBefore is how long before a signed metadata role expires the operator is told. It sits well above the
// refresh cadence of each role (timestamp 24 h lifetime re-signed every 6 h, snapshot 7 d re-signed with it, targets
// 90 d re-signed on every publish or rollout change, root 2 years offline), so a warning means the refresh is not
// happening. Prior art: TUF spec 5.3.10 (expiry is the freshness guarantee a client enforces).
var ExpiryWarnBefore = map[string]time.Duration{
	"timestamp": 12 * time.Hour,
	"snapshot":  3 * 24 * time.Hour,
	"targets":   14 * 24 * time.Hour,
	"root":      90 * 24 * time.Hour,
}

// ExpiryWarning names a role that expires within its warning window (or already has).
type ExpiryWarning struct {
	Role    string
	Expires time.Time
	Expired bool
}

// ReadMetadataExpiry reads each role's expiry from the published repository. It does not verify signatures: clients
// do, and this only watches the clock.
func ReadMetadataExpiry(directory string) (map[string]time.Time, error) {
	expiries := make(map[string]time.Time, len(ExpiryWarnBefore))
	for role := range ExpiryWarnBefore {
		body, err := os.ReadFile(filepath.Join(directory, "tuf", "metadata", role+".json"))
		if err != nil {
			return nil, fmt.Errorf("read %s metadata: %w", role, err)
		}
		var doc struct {
			Signed struct {
				Expires time.Time `json:"expires"`
			} `json:"signed"`
		}
		if err := json.Unmarshal(body, &doc); err != nil || doc.Signed.Expires.IsZero() {
			return nil, fmt.Errorf("%s metadata has no readable expiry", role)
		}
		expiries[role] = doc.Signed.Expires
	}
	return expiries, nil
}

// ExpiryWarnings lists the roles inside their warning window at now, soonest first.
func ExpiryWarnings(expiries map[string]time.Time, now time.Time) []ExpiryWarning {
	var warnings []ExpiryWarning
	for role, expires := range expiries {
		if window, ok := ExpiryWarnBefore[role]; ok && expires.Sub(now) < window {
			warnings = append(warnings, ExpiryWarning{Role: role, Expires: expires, Expired: !expires.After(now)})
		}
	}
	sort.Slice(warnings, func(i, j int) bool { return warnings[i].Expires.Before(warnings[j].Expires) })
	return warnings
}

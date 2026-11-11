package peersessions

import (
	"log/slog"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// AdmittedScope identifies work already admitted by this authenticated endpoint.
// Requesting it never renews its credential or grants new admission authority.
type AdmittedScope struct {
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
}

// admittedScopeIDs groups the reported scopes by kind. A kind this server does not know (a newer client's) and a
// repeat (a retry after a lost answer) are skipped, never a reason to refuse the endpoint its whole configuration;
// a malformed id or more than 128 references still are.
func admittedScopeIDs(scopes []AdmittedScope) (map[string][]string, error) {
	if len(scopes) > 128 {
		return nil, ErrInvalid
	}
	ids := map[string][]string{}
	peernetwork.Scopes(func(kind, _ string) bool { ids[kind] = []string{}; return true }) // every kind the client may track
	seen := map[AdmittedScope]bool{}
	for _, scope := range scopes {
		if !bounded(scope.ResourceID, 1, peernetwork.MaxResourceIDBytes) {
			return nil, ErrInvalid
		}
		if _, ok := ids[scope.ResourceKind]; !ok {
			slog.Warn("peer network configuration skipped an admitted scope of an unknown kind", "site", "admitted_scopes.go")
			continue
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		ids[scope.ResourceKind] = append(ids[scope.ResourceKind], scope.ResourceID)
	}
	return ids, nil
}

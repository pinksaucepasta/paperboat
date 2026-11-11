package peersessions

import "github.com/pinksaucepasta/paperboat/shared/peernetwork"

// AdmittedScope identifies work already admitted by this authenticated endpoint.
// Requesting it never renews its credential or grants new admission authority.
type AdmittedScope struct {
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
}

func admittedScopeIDs(scopes []AdmittedScope) (map[string][]string, error) {
	if len(scopes) > 128 {
		return nil, ErrInvalid
	}
	ids := map[string][]string{}
	peernetwork.Scopes(func(kind, _ string) bool { ids[kind] = []string{}; return true }) // every kind the client may track
	seen := map[AdmittedScope]bool{}
	for _, scope := range scopes {
		if _, ok := ids[scope.ResourceKind]; !ok || !bounded(scope.ResourceID, 1, peernetwork.MaxResourceIDBytes) || seen[scope] {
			return nil, ErrInvalid
		}
		seen[scope] = true
		ids[scope.ResourceKind] = append(ids[scope.ResourceKind], scope.ResourceID)
	}
	return ids, nil
}

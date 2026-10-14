package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelfhostInventoryQueryBoundsAndFilters(t *testing.T) {
	for _, query := range []string{"scope_kind=bogus", "scope_id=foreign", "scope_kind=team&scope_id=" + strings.Repeat("x", 129), "scope_kind=team&scope_id=a&scope_id=b", "limit=0", "limit=201", "limit=", "offset=-1", "offset=2147483648", "limit=10&limit=20", "q=a&q=b", "capability=ssh", "enrollment_state=active", "ready=1", "ready=", "workspace=personal", "owner=mine", "q=%00", "q=" + strings.Repeat("x", 161)} {
		if _, err := selfhostInventoryOptions(httptest.NewRequest("GET", "/v1/selfhost/installations?"+query, nil)); err == nil {
			t.Fatalf("invalid query accepted: %s", query)
		}
	}
	options, err := selfhostInventoryOptions(httptest.NewRequest("GET", "/v1/selfhost/installations?scope_kind=team&scope_id=team_1&limit=200&offset=200&q=Edge&capability=tunnel&enrollment_state=enrolled&ready=false", nil))
	if err != nil || options.Limit != 200 || options.Offset != 200 || options.Q != "Edge" || options.Capability != "tunnel" || options.EnrollmentState != "enrolled" || options.Ready == nil || *options.Ready {
		t.Fatalf("effective options: %+v %v", options, err)
	}
	defaults, err := selfhostInventoryOptions(httptest.NewRequest("GET", "/v1/selfhost/installations", nil))
	if err != nil || defaults.Limit != 50 || defaults.Ready != nil {
		t.Fatalf("default options: %+v %v", defaults, err)
	}
}

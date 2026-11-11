package tunnelv1

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A unique violation is classified by its exact constraint name, so a constraint whose name merely contains one of
// them is a plain conflict, not a name, route or domain conflict.
func TestTranslateClassifiesUniqueViolationsByExactConstraint(t *testing.T) {
	for name, want := range map[string]error{
		"tunnels_account_id_name_personal":     ErrNameConflict,
		"tunnels_account_id_name_team":         ErrNameConflict,
		"tunnel_routes_tunnel_id_name_key":     ErrRouteConflict,
		"tunnel_routes_id_tunnel_unique":       ErrRouteConflict,
		"tunnel_domains_live_hostname_unique":  ErrDomainConflict,
		"tunnels_stable_endpoint_key":          ErrEndpointConflict,
		"tunnels_account_id_name_future_extra": ErrConflict,
		"x_tunnel_routes_tunnel_id_name_key_2": ErrConflict,
		"tunnel_domains_hostname_other":        ErrConflict,
	} {
		err := translate(&pgconn.PgError{Code: "23505", ConstraintName: name})
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
		if want == ErrConflict && (errors.Is(err, ErrNameConflict) || errors.Is(err, ErrRouteConflict) || errors.Is(err, ErrDomainConflict) || errors.Is(err, ErrEndpointConflict)) {
			t.Errorf("%s was misclassified: %v", name, err)
		}
	}
}

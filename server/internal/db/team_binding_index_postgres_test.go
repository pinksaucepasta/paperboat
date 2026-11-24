package db

import "testing"

// Sharing lookups name the resource and not the team; without this index each one read every binding.
func TestTeamResourceBindingsIndexedByActiveResourceOnPostgres(t *testing.T) {
	f := newCeilingTeam(t)
	if f.value(`SELECT count(*) FROM pg_index WHERE indrelid='paperboat.team_resource_bindings'::regclass AND indisvalid
		AND pg_get_indexdef(indexrelid) LIKE '%(resource_kind, resource_id) INCLUDE (team_id, generation) WHERE active'`) != 1 {
		t.Fatal("team_resource_bindings has no valid index on active (resource_kind, resource_id)")
	}
}

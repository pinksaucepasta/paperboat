package selfhost

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// migrationUp runs the Up section of a migration file as the database would, without the goose directives.
func migrationUp(t *testing.T, f *pairFixture, name string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	// One statement per exec, as goose runs a NO TRANSACTION file: a DO block that commits per batch cannot run
	// inside the implicit transaction of a multi-statement exec.
	var stmt []string
	block := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "-- +goose Down"):
			return
		case strings.HasPrefix(line, "-- +goose StatementBegin"):
			block = true
		case strings.HasPrefix(line, "-- +goose StatementEnd"):
			block = false
			f.exec(strings.Join(stmt, "\n"))
			stmt = nil
		case strings.HasPrefix(line, "-- +goose"):
		default:
			stmt = append(stmt, line)
			if !block && strings.HasSuffix(strings.TrimSpace(line), ";") {
				f.exec(strings.Join(stmt, "\n"))
				stmt = nil
			}
		}
	}
}

// A pair's regions fix the tunnel pool: the fixup migration adds a sibling server of the pair's region that was enrolled
// before the regions existed, and never a server of another region. Rerunning changes nothing.
func TestPoolFixupMigrationAddsSiblingsInThePairRegions(t *testing.T) {
	f := newPairFixture(t)
	// The owner's pair is its own region f.own1 (node n1), and its policy lists only s1 (the server of n1).
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,node_ids,regions) VALUES('account',$1,'self-hosted-only',ARRAY[$2],ARRAY[$3])`, f.owner, f.n1, f.own1)
	f.exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, f.owner, f.s1)
	installations := func() []string {
		var ids []string
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT installation_ids FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1 AND capability='tunnel'`, f.owner).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		return sorted(ids...)
	}
	migrationUp(t, f, "488_tunnel_pool_follows_regions.sql")
	// s1b is enrolled on n1b, another node of the same region; s2 is in the other region and must stay out.
	if got, want := installations(), sorted(f.s1, f.s1b); !slices.Equal(got, want) {
		t.Fatalf("pool after the fixup=%v, want %v", got, want)
	}
	var candidate bool
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT paperboat.node_workspace_edge_candidate($1,$2,NULL)`, f.n1b, f.owner).Scan(&candidate); err != nil || !candidate {
		t.Fatalf("sibling node candidate=%v %v, want true", candidate, err)
	}
	migrationUp(t, f, "488_tunnel_pool_follows_regions.sql")
	if got, want := installations(), sorted(f.s1, f.s1b); !slices.Equal(got, want) {
		t.Fatalf("pool after a rerun=%v, want %v", got, want)
	}
}

// A pair whose regions hold no active server of the owner must not empty the owner's pool: self-hosted-only needs a
// server, and the edge-set worker (not this fixup) replaces a pair that points nowhere.
func TestPoolFixupMigrationNeverEmptiesAPool(t *testing.T) {
	f := newPairFixture(t)
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,node_ids,regions) VALUES('account',$1,'self-hosted-only',ARRAY[$2],ARRAY[$3])`, f.owner, f.n1, "nowhere_"+f.stamp)
	f.exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'tunnel','self-hosted-only',ARRAY[$2])`, f.owner, f.s1)
	migrationUp(t, f, "488_tunnel_pool_follows_regions.sql")
	var ids []string
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT installation_ids FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1 AND capability='tunnel'`, f.owner).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{f.s1}) {
		t.Fatalf("pool after the fixup=%v, want the owner's list %v kept", ids, []string{f.s1})
	}
}

// The region backfill walks every pair in batches of 1000, so 2500 pairs take several batches and the walk still ends
// with the rows whose nodes name no region left empty. Rerunning changes no resolved pair.
func TestRegionBackfillMigrationIsBatchedAndTerminates(t *testing.T) {
	f := newPairFixture(t)
	prefix := "backfill-" + f.stamp
	t.Cleanup(func() {
		if _, err := f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.workspace_edge_sets WHERE scope_id LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	// The pair names the server of n2 (region f.own2), then n1 and n1b (both in f.own1): the regions are own2 then own1.
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,node_ids) SELECT 'account', $1||'-ok-'||g, 'self-hosted-only', ARRAY[$2,$3,$4] FROM generate_series(1,2500) g`, prefix, f.n2, f.n1, f.n1b)
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,node_ids) SELECT 'account', $1||'-missing-'||g, 'self-hosted-only', ARRAY[$2] FROM generate_series(1,3) g`, prefix, "missing-"+f.stamp)
	count := func(query string, args ...any) int {
		var n int
		if err := f.store.Pool().QueryRow(f.ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	migrationUp(t, f, "484_workspace_edge_set_regions_backfill.sql")
	if got := count(`SELECT count(*) FROM paperboat.workspace_edge_sets WHERE scope_id LIKE $1 AND scope_id LIKE '%-ok-%' AND regions=ARRAY[$2,$3]::text[]`, prefix+"%", f.own2, f.own1); got != 2500 {
		t.Fatalf("pairs with regions [own2 own1] = %d, want 2500", got)
	}
	if got := count(`SELECT count(*) FROM paperboat.workspace_edge_sets WHERE scope_id LIKE $1 AND scope_id LIKE '%-missing-%' AND cardinality(regions)=0`, prefix+"%"); got != 3 {
		t.Fatalf("unresolvable pairs left empty = %d, want 3", got)
	}
	var before, after int64
	sumXmin := `SELECT coalesce(sum(xmin::text::bigint),0) FROM paperboat.workspace_edge_sets WHERE scope_id LIKE $1 AND scope_id LIKE '%-ok-%'`
	if err := f.store.Pool().QueryRow(f.ctx, sumXmin, prefix+"%").Scan(&before); err != nil {
		t.Fatal(err)
	}
	migrationUp(t, f, "484_workspace_edge_set_regions_backfill.sql")
	if err := f.store.Pool().QueryRow(f.ctx, sumXmin, prefix+"%").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("rerun rewrote resolved pairs (xmin sum %d -> %d)", before, after)
	}
}

package selfhost

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestRegionSlugBase(t *testing.T) {
	for name, want := range map[string]string{
		"Home lab":                     "home-lab",
		"  Home   lab  ":               "home-lab",
		"Ünïcode":                      "unicode",
		"Lab—West":                     "lab-west",
		"Eu_West 1!":                   "eu-west-1",
		"123":                          "r-123",
		"9 lives":                      "r-9-lives",
		"!!!":                          "r",
		"日本":                           "r",
		"a日b":                          "a-b",
		"-edge-":                       "edge",
		"ＦＵＬＬ　ｗｉｄｔｈ":                   "full-width",
		strings.Repeat("a", 60):        strings.Repeat("a", 40),
		strings.Repeat("a", 39) + " b": strings.Repeat("a", 39),
		"selfhost":                     "selfhost",
	} {
		got := regionSlugBase(name)
		if got != want {
			t.Errorf("regionSlugBase(%q)=%q, want %q", name, got, want)
		}
		if !validRegion(got) {
			t.Errorf("regionSlugBase(%q)=%q is not a valid region", name, got)
		}
	}
	if got := uniqueRegionSlug("home-lab", map[string]bool{"home-lab": true, "home-lab-2": true}); got != "home-lab-3" {
		t.Errorf("collision suffix: %q", got)
	}
}

func TestRegionNameValidation(t *testing.T) {
	for _, bad := range []string{"", "   ", strings.Repeat("a", 49), "tab\there", "new\nline", "bell\x07", "zero​width", "bad\xffutf8", "line sep"} {
		if _, err := validRegionName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q accepted: %v", bad, err)
		}
	}
	for _, good := range []string{"Home lab", "a", strings.Repeat("é", 48), "Büro 2 (Köln) – primary"} {
		if got, err := validRegionName(" " + good + " "); err != nil || got != good {
			t.Errorf("name %q refused or changed: %q %v", good, got, err)
		}
	}
}

// regionNode adds a ready edge server of the account in region and returns its installation and node.
func (f *pairFixture) regionNode(name, kind, scopeID, region string) (installation, node string) {
	f.t.Helper()
	node = f.node(name, region, 0)
	return f.own(name+"i", kind, scopeID, node), node
}

func (f *pairFixture) regions(actor string, scope Scope) map[string]Region {
	f.t.Helper()
	list, err := f.service.ListRegions(f.ctx, actor, scope)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]Region{}
	for _, r := range list {
		out[r.ID] = r
	}
	return out
}

func TestCreateRegionDerivesSlugs(t *testing.T) {
	f := newPairFixture(t)
	create := func(name string) Region {
		t.Helper()
		r, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, name)
		if err != nil {
			t.Fatalf("CreateRegion(%q): %v", name, err)
		}
		return r
	}
	if r := create("Home lab"); r.ID != "home-lab" || r.Name != "Home lab" || r.Machines != 0 || r.Ready != 0 {
		t.Fatalf("Home lab: %+v", r)
	}
	if r := create("  Ünïcode  "); r.ID != "unicode" || r.Name != "Ünïcode" {
		t.Fatalf("Ünïcode: %+v", r)
	}
	if r := create("123"); r.ID != "r-123" {
		t.Fatalf("123: %+v", r)
	}
	if r := create("!!!"); r.ID != "r" {
		t.Fatalf("punctuation only: %+v", r)
	}
	if r := create("???"); r.ID != "r-2" {
		t.Fatalf("second punctuation only: %+v", r)
	}
	// The default region's slug is reserved even though nothing uses it yet; the name is free.
	if r := create("selfhost!"); r.ID != "selfhost-2" || r.Name != "selfhost!" {
		t.Fatalf("reserved slug: %+v", r)
	}
	// Same slug as an existing region, different name: the slug gets a suffix.
	if r := create("Home-lab"); r.ID != "home-lab-2" {
		t.Fatalf("slug collision: %+v", r)
	}
	if r := create("Home_lab"); r.ID != "home-lab-3" {
		t.Fatalf("second slug collision: %+v", r)
	}
	// A slug that collides with a region of the scope's servers (implicit) or with one of Paperboat's managed regions.
	f.regionNode("garage", "account", f.owner, "garage")
	if r := create("Garage!"); r.ID != "garage-2" {
		t.Fatalf("slug of an implicit region: %+v", r)
	}
	// Paperboat's region ids are never reused as an owner's slug.
	managed := "berlin-dc-" + strings.ToLower(f.stamp[:8])
	f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Berlin',true)`, managed)
	t.Cleanup(func() {
		_, _ = f.store.SQL().Exec(`DELETE FROM paperboat.operator_regions WHERE id=$1`, managed)
	})
	if r := create(strings.ReplaceAll(managed, "-", " ")); r.ID != managed+"-2" {
		t.Fatalf("slug of a managed region: %+v", r)
	}
	// What was stored reads back.
	got := f.regions(f.owner, f.personal)
	if got["home-lab"].Name != "Home lab" || got["unicode"].Name != "Ünïcode" || got["garage"].Name != "garage" || got["garage"].Machines != 1 {
		t.Fatalf("list after create: %+v", got)
	}
}

func TestCreateRegionRefusesDuplicateNames(t *testing.T) {
	f := newPairFixture(t)
	if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Home lab"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Home lab", "HOME LAB", " home lab ", "hOmE LaB"} {
		_, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, name)
		var refused *regionError
		if !errors.Is(err, ErrRegionExists) || !errors.As(err, &refused) || !strings.Contains(refused.Error(), "Home lab") {
			t.Fatalf("duplicate %q: %v", name, err)
		}
	}
	// An implicit region is named by its slug; creating a region with that name collides, and so does the default.
	f.regionNode("garage", "account", f.owner, "garage")
	for _, name := range []string{"garage", "GARAGE", "Garage", "selfhost", "SelfHost"} {
		_, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, name)
		if !errors.Is(err, ErrRegionExists) {
			t.Fatalf("implicit %q: %v", name, err)
		}
		if name == "GARAGE" && !strings.Contains(err.Error(), `"garage"`) {
			t.Fatalf("the refusal names the existing region: %v", err)
		}
	}
	// Names are per scope: another account (and the team) may use the same name.
	if _, err := f.service.CreateRegion(f.ctx, f.other, Scope{Kind: "account", ID: f.other}, "Home lab"); err != nil {
		t.Fatalf("another scope's name: %v", err)
	}
	if _, err := f.service.CreateRegion(f.ctx, f.owner, f.teamScope, "Home lab"); err != nil {
		t.Fatalf("the team's name: %v", err)
	}
	// Concurrent creates of one name: exactly one wins (the scope lock serializes them).
	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.service.CreateRegion(f.ctx, f.owner, f.personal, "Racing region")
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrRegionExists):
			t.Fatalf("racing create: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent creates of one name succeeded", wins)
	}
}

func TestCreateRegionBoundsAndLimit(t *testing.T) {
	f := newPairFixture(t)
	for _, bad := range []string{"", "  ", strings.Repeat("x", 49), "bad\x00name", "tab\tin"} {
		if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q: %v", bad, err)
		}
	}
	for i := range regionLimit {
		if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Region "+string(rune('A'+i/26))+string(rune('a'+i%26))); err != nil {
			t.Fatalf("region %d: %v", i, err)
		}
	}
	if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "One too many"); !errors.Is(err, ErrLimit) {
		t.Fatalf("past the limit: %v", err)
	}
}

func TestRegionListUnionsStoredAndImplicitRegions(t *testing.T) {
	f := newPairFixture(t)
	// The fixture's own servers: own1 has two nodes (s1, s1b), own2 one. Paperboat's regions and other scopes never appear.
	if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Zeta empty"); err != nil {
		t.Fatal(err)
	}
	got := f.regions(f.owner, f.personal)
	for _, managed := range []string{f.ra, f.rb, f.rc, f.rs, f.rd, f.teamr1, f.ownx} {
		if _, found := got[managed]; found {
			t.Fatalf("region %s of another scope is listed: %+v", managed, got)
		}
	}
	if got[f.own1].Machines != 2 || got[f.own1].Ready != 2 || got[f.own1].Name != f.own1 {
		t.Fatalf("own1: %+v", got[f.own1])
	}
	if got[f.own2].Machines != 1 || got[f.own2].Ready != 1 {
		t.Fatalf("own2: %+v", got[f.own2])
	}
	if got["zeta-empty"].Machines != 0 || got["zeta-empty"].Name != "Zeta empty" || len(got) != 3 {
		t.Fatalf("stored empty region: %+v", got)
	}
	// A machine that runs two components (two nodes, one installation key) counts once; a node that stopped reporting
	// counts but is not ready.
	relayNode := f.node("n1relay", f.own1, 0)
	f.exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete)
 SELECT $1,$2,installation_public_key,decode(md5($1||'h')||md5($1||'hh'),'hex'),'fixture','Relay of n1','relay','account',$3,$1,true FROM paperboat.selfhost_installations WHERE id=$4`, "relay_n1_"+f.stamp, relayNode, f.owner, f.s1)
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, f.n1b)
	got = f.regions(f.owner, f.personal)
	if got[f.own1].Machines != 2 || got[f.own1].Ready != 1 {
		t.Fatalf("own1 with a dual-role and a silent node: %+v", got[f.own1])
	}
	// A pending (unclaimed) installation is not listed; a removed server leaves its region without a stored row.
	pending, node := f.regionNode("pend", "account", f.owner, "pending-region")
	f.exec(`UPDATE paperboat.selfhost_installations SET claim_complete=false WHERE id=$1`, pending)
	if _, found := f.regions(f.owner, f.personal)["pending-region"]; found {
		t.Fatal("a pending claim's region is listed")
	}
	f.exec(`UPDATE paperboat.selfhost_installations SET claim_complete=true WHERE id=$1`, pending)
	if got = f.regions(f.owner, f.personal); got["pending-region"].Machines != 1 {
		t.Fatalf("claimed region: %+v", got["pending-region"])
	}
	if err := f.service.RemoveScoped(f.ctx, f.owner, pending, false); err != nil {
		t.Fatal(err)
	}
	if _, found := f.regions(f.owner, f.personal)["pending-region"]; found {
		t.Fatalf("an emptied implicit region is listed (node %s)", node)
	}
	// Ordered by display name, case-insensitively.
	list, err := f.service.ListRegions(f.ctx, f.owner, f.personal)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(list))
	for i, r := range list {
		names[i] = strings.ToLower(r.Name)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("regions are not ordered by name: %v", names)
	}
	// The team lists its own.
	team := f.regions(f.owner, f.teamScope)
	if len(team) != 2 || team[f.teamr1].Machines != 1 || team[f.teamr2].Machines != 1 {
		t.Fatalf("team regions: %+v", team)
	}
}

func TestRenameRegion(t *testing.T) {
	f := newPairFixture(t)
	created, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Home lab")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, created.ID, "  Basement ")
	if err != nil || renamed.ID != "home-lab" || renamed.Name != "Basement" {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	// The same name in another case is a rename of itself, not a clash.
	if r, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, created.ID, "BASEMENT"); err != nil || r.Name != "BASEMENT" || r.ID != "home-lab" {
		t.Fatalf("recase: %+v %v", r, err)
	}
	// The old name is free again; the slug is unchanged for later regions.
	if r, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Home lab"); err != nil || r.ID != "home-lab-2" {
		t.Fatalf("new region with the old name: %+v %v", r, err)
	}
	// An implicit region is stored by its first rename; its servers, nodes and slug do not change.
	before := f.regions(f.owner, f.personal)[f.own1]
	r, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own1, "Garage")
	if err != nil || r.ID != f.own1 || r.Name != "Garage" || r.Machines != before.Machines || r.Ready != before.Ready {
		t.Fatalf("implicit rename: %+v %v", r, err)
	}
	var nodeRegion string
	if err = f.store.SQL().QueryRowContext(f.ctx, `SELECT region FROM paperboat.control_tunnel_nodes WHERE id=$1`, f.n1).Scan(&nodeRegion); err != nil || nodeRegion != f.own1 {
		t.Fatalf("rename touched the node: %q %v", nodeRegion, err)
	}
	if got := f.regions(f.owner, f.personal)[f.own1]; got.Name != "Garage" || got.Machines != 2 {
		t.Fatalf("listed after rename: %+v", got)
	}
	// Clashes: another stored name, another region's slug (implicit name), and the default.
	for _, name := range []string{"basement", "garage", f.own2, strings.ToUpper(f.own2), "selfhost"} {
		_, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, "home-lab-2", name)
		if !errors.Is(err, ErrRegionExists) {
			t.Fatalf("rename to %q: %v", name, err)
		}
	}
	// An implicit region may take any case of its own slug as its name.
	if r, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own2, strings.ToUpper(f.own2)); err != nil || r.Name != strings.ToUpper(f.own2) {
		t.Fatalf("implicit region renamed to its slug in capitals: %+v %v", r, err)
	}
	// Unknown regions, Paperboat's regions and other scopes' regions are not found, and invalid names invalid.
	for _, id := range []string{"nope", f.ra, f.teamr1, f.ownx, "Not A Slug"} {
		if _, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, id, "Whatever"); !errors.Is(err, ErrRegionNotFound) {
			t.Fatalf("rename of %q: %v", id, err)
		}
	}
	if _, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own1, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty rename: %v", err)
	}
}

func TestDeleteRegionGuards(t *testing.T) {
	f := newPairFixture(t)
	empty, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Empty lab")
	if err != nil {
		t.Fatal(err)
	}
	// Servers: an implicit region with servers, and a named one once a server joins it.
	err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own1)
	var refused *regionError
	if !errors.Is(err, ErrRegionInUse) || !errors.As(err, &refused) || !strings.Contains(refused.Error(), "Remove them") {
		t.Fatalf("delete of a region with servers: %v", err)
	}
	if _, err = f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own1, "Garage"); err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own1); !errors.Is(err, ErrRegionInUse) || !strings.Contains(err.Error(), "Garage") {
		t.Fatalf("delete of a named region with servers: %v", err)
	}
	// A pending (unclaimed) server also holds its region.
	f.exec(`UPDATE paperboat.selfhost_installations SET claim_complete=false WHERE id=$1`, f.s2)
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own2); !errors.Is(err, ErrRegionInUse) {
		t.Fatalf("delete with a pending server: %v", err)
	}
	f.exec(`UPDATE paperboat.selfhost_installations SET claim_complete=true WHERE id=$1`, f.s2)
	// A pair that names the region (no servers left in it) holds it too.
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'self-hosted-only',ARRAY[$2,'other-region'])`, f.owner, empty.ID)
	err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, empty.ID)
	if !errors.Is(err, ErrRegionInUse) || !strings.Contains(err.Error(), "Switch their regions") {
		t.Fatalf("delete of a region a pair names: %v", err)
	}
	f.exec(`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.owner)
	// Free: stored and empty. Gone from the list, and not found the second time.
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, empty.ID); err != nil {
		t.Fatalf("delete of an empty region: %v", err)
	}
	if _, found := f.regions(f.owner, f.personal)[empty.ID]; found {
		t.Fatal("the deleted region is still listed")
	}
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, empty.ID); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// After the servers leave, the named region stays (it was stored) and can then be deleted; an implicit one just vanishes.
	for _, id := range []string{f.s1, f.s1b} {
		if err = f.service.RemoveScoped(f.ctx, f.owner, id, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.regions(f.owner, f.personal)[f.own1]; got.Name != "Garage" || got.Machines != 0 {
		t.Fatalf("a stored region after its servers left: %+v", got)
	}
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own1); err != nil {
		t.Fatalf("delete after the servers left: %v", err)
	}
	// Paperboat's regions and other scopes' are not found, never deleted.
	for _, id := range []string{f.ra, f.teamr1, "nope", "Not A Slug"} {
		if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, id); !errors.Is(err, ErrRegionNotFound) {
			t.Fatalf("delete of %q: %v", id, err)
		}
	}
}

func TestDeleteTeamRegionHeldByAMembersPersonalPair(t *testing.T) {
	f := newPairFixture(t)
	lab, err := f.service.CreateRegion(f.ctx, f.admin, f.teamScope, "Team lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.SQL().Exec(`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.member)
	})
	// A member's personal pair that names the team's region, via the team's servers it may use.
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'self-hosted-only',ARRAY[$2])`, f.member, lab.ID)
	if err = f.service.DeleteRegion(f.ctx, f.admin, f.teamScope, lab.ID); !errors.Is(err, ErrRegionInUse) {
		t.Fatalf("team region held by a personal pair: %v", err)
	}
	// The team's own pair holds it as well.
	f.exec(`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.member)
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('team',$1,'self-hosted-only',ARRAY[$2])`, f.team, lab.ID)
	if err = f.service.DeleteRegion(f.ctx, f.admin, f.teamScope, lab.ID); !errors.Is(err, ErrRegionInUse) {
		t.Fatalf("team region held by the team's pair: %v", err)
	}
	f.exec(`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.team)
	if err = f.service.DeleteRegion(f.ctx, f.admin, f.teamScope, lab.ID); err != nil {
		t.Fatalf("free team region: %v", err)
	}
}

func TestRegionAuthorization(t *testing.T) {
	f := newPairFixture(t)
	region, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	teamRegion, err := f.service.CreateRegion(f.ctx, f.owner, f.teamScope, "Team's")
	if err != nil {
		t.Fatal(err)
	}
	// A team admin may write; a plain member reads only; an account outside the team does neither.
	if _, err = f.service.CreateRegion(f.ctx, f.admin, f.teamScope, "Admin's"); err != nil {
		t.Fatalf("team admin create: %v", err)
	}
	if got := f.regions(f.member, f.teamScope); got[teamRegion.ID].Name != "Team's" {
		t.Fatalf("a member reads the team's regions: %+v", got)
	}
	denied := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s: %v", what, err)
		}
	}
	_, err = f.service.CreateRegion(f.ctx, f.member, f.teamScope, "Member's")
	denied("member create", err)
	_, err = f.service.RenameRegion(f.ctx, f.member, f.teamScope, teamRegion.ID, "Renamed")
	denied("member rename", err)
	denied("member delete", f.service.DeleteRegion(f.ctx, f.member, f.teamScope, teamRegion.ID))
	_, err = f.service.ListRegions(f.ctx, f.other, f.teamScope)
	denied("outsider list of the team", err)
	_, err = f.service.CreateRegion(f.ctx, f.other, f.teamScope, "Outsider")
	denied("outsider create in the team", err)
	denied("outsider delete in the team", f.service.DeleteRegion(f.ctx, f.other, f.teamScope, teamRegion.ID))
	// Another account's personal scope is closed, for reads and writes, and the operator's global scope too.
	_, err = f.service.ListRegions(f.ctx, f.other, f.personal)
	denied("other list of the owner's regions", err)
	_, err = f.service.CreateRegion(f.ctx, f.other, f.personal, "Intruder")
	denied("other create in the owner's scope", err)
	_, err = f.service.RenameRegion(f.ctx, f.other, f.personal, region.ID, "Hijack")
	denied("other rename", err)
	denied("other delete", f.service.DeleteRegion(f.ctx, f.other, f.personal, region.ID))
	_, err = f.service.ListRegions(f.ctx, f.owner, Scope{Kind: "global"})
	denied("global list", err)
	_, err = f.service.CreateRegion(f.ctx, f.owner, Scope{Kind: "global"}, "Global")
	denied("global create", err)
	// The default scope is the actor's own account.
	if _, err = f.service.CreateRegion(f.ctx, f.other, Scope{}, "Default scope"); err != nil {
		t.Fatalf("default scope create: %v", err)
	}
	if _, err = f.service.ListRegions(f.ctx, f.owner, Scope{Kind: "bogus"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown scope kind: %v", err)
	}
	// Nothing leaked into the owner's scope.
	got := f.regions(f.owner, f.personal)
	if _, found := got["intruder"]; found || got[region.ID].Name != "Mine" {
		t.Fatalf("owner's regions after refused writes: %+v", got)
	}
}

package edgeset

import (
	"reflect"
	"testing"
)

func candidateList(pairs ...string) []Candidate {
	out := make([]Candidate, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Candidate{NodeID: pairs[i], FailureDomain: pairs[i+1]})
	}
	return out
}

func retainedOf(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func TestChooseSpreadsAcrossFailureDomains(t *testing.T) {
	usable := candidateList("a1", "dom-a", "a2", "dom-a", "b1", "dom-b")
	got := choose("scope", nil, nil, usable, Size)
	if len(got) != 2 {
		t.Fatalf("set = %v", got)
	}
	domains := map[string]string{"a1": "dom-a", "a2": "dom-a", "b1": "dom-b"}
	if domains[got[0]] == domains[got[1]] {
		t.Fatalf("set %v shares a failure domain although two exist", got)
	}
}

func TestChooseIsStableAndKeepsRetainedMembers(t *testing.T) {
	usable := candidateList("a", "dom-a", "b", "dom-b", "c", "dom-c")
	first := choose("scope", nil, nil, usable, Size)
	if again := choose("scope", nil, nil, usable, Size); !reflect.DeepEqual(first, again) {
		t.Fatalf("same inputs gave %v then %v", first, again)
	}
	if kept := choose("scope", first, retainedOf(first...), usable, Size); !reflect.DeepEqual(kept, first) {
		t.Fatalf("a healthy set changed: %v -> %v", first, kept)
	}
	// One member is gone: the other keeps its place, one replacement joins from another domain.
	survivor := first[1]
	var remaining []Candidate
	for _, candidate := range usable {
		if candidate.NodeID != first[0] {
			remaining = append(remaining, candidate)
		}
	}
	replaced := choose("scope", first, retainedOf(survivor), remaining, Size)
	if len(replaced) != 2 || replaced[0] != survivor || replaced[1] == first[0] {
		t.Fatalf("replacement %v after losing %s from %v", replaced, first[0], first)
	}
}

func TestChooseFallsBackToOneOrSameDomain(t *testing.T) {
	if got := choose("scope", nil, nil, candidateList("only", "dom"), Size); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("single edge set = %v", got)
	}
	if got := choose("scope", nil, nil, candidateList("x", "dom", "y", "dom"), Size); len(got) != 2 {
		t.Fatalf("two edges in one domain should still form a set, got %v", got)
	}
	if got := choose("scope", nil, nil, nil, Size); len(got) != 0 {
		t.Fatalf("no edges gave %v", got)
	}
}

func TestChooseSpreadsWorkspacesAcrossEdges(t *testing.T) {
	usable := candidateList("a", "dom-a", "b", "dom-b", "c", "dom-c", "d", "dom-d")
	first := map[string]int{}
	for i := 0; i < 200; i++ {
		got := choose("workspace-"+string(rune('A'+i%26))+string(rune('a'+i/26)), nil, nil, usable, 1)
		first[got[0]]++
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if first[id] < 20 {
			t.Fatalf("edge %s received only %d of 200 workspaces: %v", id, first[id], first)
		}
	}
}

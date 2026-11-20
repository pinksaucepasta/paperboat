package edgeset

import (
	"reflect"
	"testing"
)

func retainedOf(regions ...string) map[string]bool {
	out := map[string]bool{}
	for _, region := range regions {
		out[region] = true
	}
	return out
}

func TestChooseGivesTwoDistinctRegions(t *testing.T) {
	got := choose("scope", nil, nil, nil, []string{"frankfurt", "mumbai", "oregon"}, Size)
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("pair = %v, want two different regions", got)
	}
}

func TestChooseIsStableAndKeepsRetainedRegions(t *testing.T) {
	usable := []string{"a", "b", "c"}
	first := choose("scope", nil, nil, nil, usable, Size)
	if again := choose("scope", nil, nil, nil, usable, Size); !reflect.DeepEqual(first, again) {
		t.Fatalf("same inputs gave %v then %v", first, again)
	}
	if kept := choose("scope", first, retainedOf(first...), nil, usable, Size); !reflect.DeepEqual(kept, first) {
		t.Fatalf("a healthy pair changed: %v -> %v", first, kept)
	}
	// One region is gone: the other keeps its place, one replacement joins.
	survivor := first[1]
	var remaining []string
	for _, region := range usable {
		if region != first[0] {
			remaining = append(remaining, region)
		}
	}
	replaced := choose("scope", first, retainedOf(survivor), nil, remaining, Size)
	if len(replaced) != 2 || replaced[0] != survivor || replaced[1] == first[0] {
		t.Fatalf("replacement %v after losing %s from %v", replaced, first[0], first)
	}
}

func TestChooseFallsBackToOneRegion(t *testing.T) {
	if got := choose("scope", nil, nil, nil, []string{"only"}, Size); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("single region pair = %v", got)
	}
	if got := choose("scope", nil, nil, nil, nil, Size); len(got) != 0 {
		t.Fatalf("no regions gave %v", got)
	}
}

func TestChooseSpreadsWorkspacesAcrossRegions(t *testing.T) {
	usable := []string{"a", "b", "c", "d"}
	first := map[string]int{}
	for i := 0; i < 200; i++ {
		got := choose("workspace-"+string(rune('A'+i%26))+string(rune('a'+i/26)), nil, nil, nil, usable, 1)
		first[got[0]]++
	}
	for _, region := range usable {
		if first[region] < 20 {
			t.Fatalf("region %s received only %d of 200 workspaces: %v", region, first[region], first)
		}
	}
}

func TestChoosePicksNearestRegions(t *testing.T) {
	usable := []string{"near", "mid", "far", "unmeasured"}
	rtt := map[string]int{"near": 10, "mid": 40, "far": 200}
	if got := choose("scope", nil, nil, rtt, usable, Size); !reflect.DeepEqual(got, []string{"near", "mid"}) {
		t.Fatalf("nearest pair = %v, want the two fastest", got)
	}
}

func TestChooseWithoutSamplesKeepsHashOrder(t *testing.T) {
	usable := []string{"a", "b", "c"}
	if got, want := choose("scope", nil, nil, map[string]int{}, usable, Size), choose("scope", nil, nil, nil, usable, Size); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty samples gave %v, hash gave %v", got, want)
	}
}

func TestChooseKeepsRetainedPairOverNearer(t *testing.T) {
	usable := []string{"slow1", "slow2", "fast"}
	rtt := map[string]int{"slow1": 300, "slow2": 400, "fast": 1}
	manual := []string{"slow1", "slow2"}
	if got := choose("scope", manual, retainedOf(manual...), rtt, usable, Size); !reflect.DeepEqual(got, manual) {
		t.Fatalf("a retained pair changed to %v", got)
	}
}

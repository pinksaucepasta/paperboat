package controlplane

import (
	"testing"
)

// The digest is a wire contract with the relay: this vector is the canonical lines hashed with sha256sum.
func TestRelaySubjectsDigestIsTheCanonicalLinesHash(t *testing.T) {
	const want = "53a1c8f1b687201c198a9cb4469859eed3d72f68e63822adeadf778c6bab04a1"
	// Sent out of order: the set sorts by (account_id, endpoint_id) before it is digested.
	set, err := newRelaySubjectSet([]relaySubject{
		{AccountID: "acct_b", EndpointID: "ep_a", Generation: 2},
		{AccountID: "acct_a", EndpointID: "ep_z", Generation: 1},
		{AccountID: "acct_a", EndpointID: "ep_b", Generation: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if set.digest != want {
		t.Fatalf("digest = %s, want %s", set.digest, want)
	}
	if set.list[0].EndpointID != "ep_b" || set.list[2].AccountID != "acct_b" {
		t.Fatalf("set is not sorted: %+v", set.list)
	}
	if set.json != `[{"account_id":"acct_a","endpoint_id":"ep_b","generation":3},{"account_id":"acct_a","endpoint_id":"ep_z","generation":1},{"account_id":"acct_b","endpoint_id":"ep_a","generation":2}]` {
		t.Fatalf("json = %s", set.json)
	}
}

func TestRelaySubjectsDigestChangesWithGenerationAndEmptySet(t *testing.T) {
	one, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep", Generation: 1}})
	bumped, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep", Generation: 2}})
	empty, _ := newRelaySubjectSet(nil)
	if one.digest == bumped.digest || one.digest == empty.digest {
		t.Fatal("digest does not distinguish the generation or the empty set")
	}
	// A nil list is the empty set: it is encoded as [] so the stored column is always an array.
	if empty.json != "[]" {
		t.Fatalf("empty json = %s", empty.json)
	}
}

func TestRelaySubjectsDigestFormat(t *testing.T) {
	valid := "53a1c8f1b687201c198a9cb4469859eed3d72f68e63822adeadf778c6bab04a1"
	for name, digest := range map[string]string{
		"short":     valid[1:],
		"uppercase": "53A1C8F1B687201C198A9CB4469859EED3D72F68E63822ADEADF778C6BAB04A1",
		"non-hex":   valid[1:] + "g",
	} {
		if validRelaySubjectsDigest(digest) {
			t.Fatalf("%s digest %q accepted", name, digest)
		}
	}
	if !validRelaySubjectsDigest(valid) {
		t.Fatal("valid digest rejected")
	}
}

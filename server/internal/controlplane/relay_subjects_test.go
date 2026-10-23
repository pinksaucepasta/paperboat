package controlplane

import (
	"testing"
)

// The digest is a wire contract with the relay: this vector is the sorted membership lines hashed with sha256sum
// (printf 'acct_a\0ep_b\n' 'acct_a\0ep_z\n' 'acct_b\0ep_a\n').
func TestRelaySubjectsDigestIsTheSortedMembershipLinesHash(t *testing.T) {
	const want = "2c9bbe3e9c2b06d2aca7a694fa3c83370dc4dc7145c822e27c318e3e90f7607d"
	// Sent out of order: the digest sorts the lines before hashing.
	set, err := newRelaySubjectSet([]relaySubject{
		{AccountID: "acct_b", EndpointID: "ep_a"},
		{AccountID: "acct_a", EndpointID: "ep_z"},
		{AccountID: "acct_a", EndpointID: "ep_b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if set.digest != want {
		t.Fatalf("digest = %s, want %s", set.digest, want)
	}
	if set.json != `[{"account_id":"acct_b","endpoint_id":"ep_a"},{"account_id":"acct_a","endpoint_id":"ep_z"},{"account_id":"acct_a","endpoint_id":"ep_b"}]` {
		t.Fatalf("json = %s", set.json)
	}
}

// Grant generations change on every peer-network refresh; the membership digest must not.
func TestRelaySubjectsDigestIgnoresGenerationsAndSeesMembership(t *testing.T) {
	steady, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep", Generation: 1}})
	refreshed, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep", Generation: 2}})
	member, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep"}})
	if steady.digest != refreshed.digest || steady.digest != member.digest {
		t.Fatal("the digest changed with a grant generation; it must name membership only")
	}
	other, _ := newRelaySubjectSet([]relaySubject{{AccountID: "acct", EndpointID: "ep2"}})
	empty, _ := newRelaySubjectSet(nil)
	if steady.digest == other.digest || steady.digest == empty.digest {
		t.Fatal("the digest does not distinguish membership")
	}
	// A nil list is the empty set: it is encoded as [] so the stored column is always an array.
	if empty.json != "[]" {
		t.Fatalf("empty json = %s", empty.json)
	}
}

func TestRelaySubjectsDigestFormat(t *testing.T) {
	valid := "2c9bbe3e9c2b06d2aca7a694fa3c83370dc4dc7145c822e27c318e3e90f7607d"
	for name, digest := range map[string]string{
		"short":     valid[1:],
		"uppercase": "2C9BBE3E9C2B06D2ACA7A694FA3C83370DC4DC7145C822E27C318E3E90F7607D",
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

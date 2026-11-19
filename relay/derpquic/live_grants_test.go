package derpquic

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// AuthoritySubjects answers from one pass over the peers; it must name exactly the subjects the per-fence liveGrantLocked walk named.
func TestAuthoritySubjectsMatchesPerFenceLiveGrantWalk(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 300; i++ {
		k := key.NewNode().Public()
		g := Grant{}
		g.AccountID, g.EndpointID, g.WireGuardPublicKey = fmt.Sprintf("account_%d", i%7), fmt.Sprintf("endpoint_%d", i), KeyString(k)
		g.ExpiresAt = now + 60
		switch i % 3 {
		case 0: // connected, grant expired: named only because it is live
			g.ExpiresAt = now - 10
			s.peers[k] = &peerConn{key: k, grant: g}
		case 1: // not connected, grant expired: not named
			g.ExpiresAt = now - 10
		default: // not connected, grant still valid: named
		}
		s.fences[g.AccountID+"\x00"+g.EndpointID] = &grantFence{Grant: g, retainUntil: now + 3600}
	}
	s.fences["account_x\x00endpoint_bad"] = &grantFence{Grant: Grant{}, retainUntil: now + 3600} // no account: never named
	want := map[AuthoritySubject]bool{}
	s.mu.Lock()
	for _, fence := range s.fences {
		g := fence.Grant
		live := s.liveGrantLocked(g)
		if g.AccountID != "" && (g.ExpiresAt > time.Now().Unix() || live != nil && grantID(live.grant) == grantID(g)) {
			want[AuthoritySubject{g.AccountID, g.EndpointID}] = true
		}
	}
	s.mu.Unlock()
	got := s.AuthoritySubjects()
	sort.Slice(got, func(i, j int) bool { return got[i].EndpointID < got[j].EndpointID })
	if len(got) != len(want) || len(want) < 150 {
		t.Fatalf("AuthoritySubjects named %d subjects, the per-fence walk %d", len(got), len(want))
	}
	for _, subject := range got {
		if !want[subject] {
			t.Fatalf("AuthoritySubjects named %+v, the per-fence walk did not", subject)
		}
	}
}

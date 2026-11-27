package machineguard

import (
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestClaimsGiveTwoUsersTheSameSlotAtDifferentAddresses(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	slot := netip.MustParseAddr("127.100.0.3")
	bob, err := book.assign(claimKey{UID: "1001", Account: "acct_bob", Machine: "m_hp"}, slot, now)
	if err != nil || bob != "127.100.0.3" {
		t.Fatalf("bob=%q err=%v", bob, err)
	}
	alice, err := book.assign(claimKey{UID: "1002", Account: "acct_alice", Machine: "m_hp2"}, slot, now)
	if err != nil || alice == bob || !validClaimAddress(netip.MustParseAddr(alice)) {
		t.Fatalf("alice=%q err=%v", alice, err)
	}
	again, _ := book.assign(claimKey{UID: "1002", Account: "acct_alice", Machine: "m_hp2"}, slot, now.Add(time.Hour))
	if again != alice {
		t.Fatalf("alice's address moved: %q -> %q", alice, again)
	}
}

func TestReleasedClaimsAreHeldThenFreed(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	key := claimKey{UID: "1001", Account: "acct", Machine: "m1"}
	ip, _ := book.assign(key, netip.MustParseAddr("127.100.0.9"), now)
	if err := book.releaseAbsent("1001", "acct", map[string]bool{"other": true}, now); err != nil {
		t.Fatal(err)
	}
	other, _ := book.assign(claimKey{UID: "1002", Account: "acct2", Machine: "m2"}, netip.MustParseAddr(ip), now.Add(time.Hour))
	if other == ip {
		t.Fatal("a held address was given to another user inside the hold")
	}
	back, _ := book.assign(key, netip.MustParseAddr("127.100.0.50"), now.Add(2*time.Hour))
	if back != ip {
		t.Fatalf("the returning machine lost its address: %q -> %q", ip, back)
	}
	_ = book.releaseAbsent("1001", "acct", map[string]bool{"other": true}, now.Add(3*time.Hour))
	after, _ := book.assign(claimKey{UID: "1003", Account: "acct3", Machine: "m3"}, netip.MustParseAddr(ip), now.Add(3*time.Hour+claimHold))
	if after != ip {
		t.Fatalf("the address was not freed after the hold: got %q", after)
	}
}

// 1: a uid whose machines come and go must never fill its cap for good.
func TestClaimsOfMachinesThatCameAndWentDoNotFillTheCap(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	for i := 0; i < 600; i++ {
		at := now.Add(time.Duration(i) * claimHold / 50)
		machine := "m" + strconv.Itoa(i)
		if _, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: machine}, netip.MustParseAddr("127.100.0.2"), at); err != nil {
			t.Fatalf("machine %d refused: %v", i, err)
		}
		if err := book.releaseAbsent("1001", "acct", map[string]bool{machine: true}, at); err != nil {
			t.Fatal(err)
		}
		// Another user acquiring meanwhile never gets an address still active or held for uid 1001.
		mine := map[string]bool{}
		for _, address := range book.addresses("1001") {
			mine[address] = true
		}
		got, err := book.assign(claimKey{UID: "1002", Account: "acct_b", Machine: machine}, netip.MustParseAddr("127.100.0.2"), at)
		if err != nil || mine[got] {
			t.Fatalf("round %d: uid 1002 got %q (%v), an address of uid 1001", i, got, err)
		}
		_ = book.releaseAbsent("1002", "acct_b", map[string]bool{machine: true}, at)
	}
}

// 3: an empty set while claims exist is never applied (a failed or empty server answer).
func TestEmptyMachineSetReleasesNothing(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	ip, _ := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m1"}, netip.MustParseAddr("127.100.0.4"), now)
	if err := book.releaseAbsent("1001", "acct", map[string]bool{}, now); err == nil {
		t.Fatal("an empty set was applied")
	}
	if got, _ := book.assign(claimKey{UID: "1002", Account: "x", Machine: "y"}, netip.MustParseAddr(ip), now.Add(claimHold*2)); got == ip {
		t.Fatal("a claim was released by an empty set")
	}
}

// smoke 1126 local access wp1: a re-paired machine (new record, same server address) got another address while
// the removed record's claim was held, so pb machine list showed an address nothing listened on.
func TestRepairedMachineTakesTheAddressOfItsReleasedRecord(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	slot := netip.MustParseAddr("127.100.0.32")
	old, _ := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_old"}, slot, now)
	if err := book.releaseAbsent("1001", "acct", map[string]bool{"m_new": true}, now); err != nil {
		t.Fatal(err)
	}
	fresh, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_new"}, slot, now.Add(time.Minute))
	if err != nil || fresh != old {
		t.Fatalf("re-paired machine got %q (err %v), want %q", fresh, err, old)
	}
	if _, ok := book.Claims[claimKey{UID: "1001", Account: "acct", Machine: "m_old"}.String()]; ok {
		t.Fatal("the removed record still holds a claim")
	}
	_ = book.releaseAbsent("1001", "acct", map[string]bool{"x": true}, now.Add(2*time.Minute))
	other, _ := book.assign(claimKey{UID: "1001", Account: "acct2", Machine: "m"}, slot, now.Add(3*time.Minute))
	if other == old {
		t.Fatal("another account took a held address inside the hold")
	}
}

// A re-pair at the user's claim cap replaces the held claim instead of failing.
func TestRepairAtTheClaimCapReplacesTheHeldClaim(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	present := map[string]bool{}
	for i := 0; i < maxUserClaims; i++ {
		m := "m" + strconv.Itoa(i)
		present[m] = true
		if _, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: m}, netip.MustParseAddr("127.100.1."+strconv.Itoa(i%250+1)), now); err != nil {
			t.Fatal(err)
		}
	}
	delete(present, "m0")
	_ = book.releaseAbsent("1001", "acct", present, now)
	held := book.Claims[claimKey{UID: "1001", Account: "acct", Machine: "m0"}.String()].IP
	got, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_new"}, netip.MustParseAddr(held), now)
	if err != nil || got != held {
		t.Fatalf("re-pair at cap: got %q err %v, want %q", got, err, held)
	}
}

// pb reset re-pairs at once: the new record can be acquired before the inventory that releases the old one. The
// old record's still-active claim yields the server's address when nothing is bound there, never while it is,
// and never to another account.
func TestResetRepairTakesTheAddressOfTheOldRecordsActiveClaim(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	bound := map[string]bool{}
	book.bound = func(ip string) bool { return bound[ip] }
	slot := netip.MustParseAddr("127.100.0.29")
	if _, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_old"}, slot, now); err != nil {
		t.Fatal(err)
	}
	bound["127.100.0.29"] = true
	if got, _ := book.assign(claimKey{UID: "1001", Account: "acct2", Machine: "m_x"}, slot, now); got == slot.String() {
		t.Fatal("another account took an active claim's address")
	}
	early, _ := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_new"}, slot, now)
	if early == slot.String() {
		t.Fatal("the new record took an address the old record's listener is still bound at")
	}
	// The old record's listener closes; the new record's next acquire (a daemon restart) moves to the address.
	bound["127.100.0.29"] = false
	got, err := book.assign(claimKey{UID: "1001", Account: "acct", Machine: "m_new"}, slot, now)
	if err != nil || got != slot.String() {
		t.Fatalf("re-pair after reset: got %q err %v, want %s", got, err, slot)
	}
	if _, still := book.Claims[claimKey{UID: "1001", Account: "acct", Machine: "m_old"}.String()]; still {
		t.Fatal("the old record's claim still holds the address")
	}
}

// A claim at another address stays there while a listener is bound at it: one machine's ports never split.
func TestClaimDoesNotMoveWhileItsListenersAreBound(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	bound := map[string]bool{}
	book.bound = func(ip string) bool { return bound[ip] }
	key := claimKey{UID: "1001", Account: "acct", Machine: "m"}
	book.Claims[claimKey{UID: "1002", Account: "other", Machine: "o"}.String()] = claim{IP: "127.100.0.29"}
	first, _ := book.assign(key, netip.MustParseAddr("127.100.0.29"), now)
	delete(book.Claims, claimKey{UID: "1002", Account: "other", Machine: "o"}.String())
	bound[first] = true
	if again, _ := book.assign(key, netip.MustParseAddr("127.100.0.29"), now); again != first {
		t.Fatalf("a bound claim moved from %s to %s", first, again)
	}
}

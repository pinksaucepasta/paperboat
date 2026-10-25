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

// Lane X 1: a uid whose machines come and go must never fill its cap for good.
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
	}
}

// Lane X 3: an empty set while claims exist is never applied (a failed or empty server answer).
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

// Lane X 2: legacy host-wide reservations are adopted entry by entry, by their own uid asking for that address.
func TestLegacyReservationsAreAdoptedPerEntry(t *testing.T) {
	book, now := newClaimBook(), time.Now()
	book.adoptLegacy(map[string]string{"127.100.0.5": "1001", "127.100.0.6": "1001"}, now)
	first, _ := book.assign(claimKey{UID: "1001", Account: "acct_a", Machine: "m5"}, netip.MustParseAddr("127.100.0.5"), now)
	second, _ := book.assign(claimKey{UID: "1001", Account: "acct_b", Machine: "m6"}, netip.MustParseAddr("127.100.0.6"), now)
	if first != "127.100.0.5" || second != "127.100.0.6" {
		t.Fatalf("legacy addresses moved: %q %q", first, second)
	}
	stranger, _ := book.assign(claimKey{UID: "1002", Account: "acct_c", Machine: "m7"}, netip.MustParseAddr("127.100.0.6"), now)
	if stranger == "127.100.0.6" {
		t.Fatal("another uid took a legacy address")
	}
	book.adoptLegacy(map[string]string{"127.100.0.8": "1001"}, now)
	late, _ := book.assign(claimKey{UID: "1003", Account: "acct_d", Machine: "m8"}, netip.MustParseAddr("127.100.0.8"), now.Add(claimHold))
	if late != "127.100.0.8" {
		t.Fatalf("an unadopted legacy entry was not released after the hold: %q", late)
	}
}

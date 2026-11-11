//go:build windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// victus, 2026-11-11: every acquire ran a full WFP Apply (enumerate every system filter, delete and re-add the
// Paperboat ones) under the guard lock, one peer after another, so ~20 peers never finished inside the daemon's
// 15 s reconcile and no native listener came up. Acquires now only bind; the daemon's one "apply" per reconcile
// installs the policy for all of them. A failed apply keeps the previous policy (the new addresses stay denied)
// and the next apply retries it.
func TestGuardAppliesThePolicyOnceForManyAcquiresAndKeepsThePreviousOneOnFailure(t *testing.T) {
	const uid = "S-1-5-21-1-2-3-1001"
	var calls int
	var failure error
	var installed []string
	previous := applyPolicy
	applyPolicy = func(_ context.Context, _ Config, leases []*guardedLease) error {
		calls++
		if failure != nil {
			return failure
		}
		installed = installed[:0]
		for _, lease := range leases {
			installed = append(installed, lease.ip)
		}
		return nil
	}
	t.Cleanup(func() { applyPolicy = previous })
	conn := &testControlConn{identity: uid}
	g := &guardServer{
		cfg:         Config{StateDir: testStateDir(t), LoopbackCIDR: "127.100.0.0/16"},
		reserved:    reservations{Names: map[string]string{}, Claims: map[string]claim{}},
		leases:      map[string]*guardedLease{},
		names:       map[controlConn]map[string]string{},
		connections: map[controlConn]string{conn: uid},
	}
	g.claims = &claimBook{Claims: g.reserved.Claims}
	acquire := func(i int) {
		t.Helper()
		in := request{Operation: "acquire", Account: "acct", Machine: fmt.Sprintf("m%d", i), Hostname: fmt.Sprintf("m%d.%s", i, testBrowserDomain), IP: fmt.Sprintf("127.100.1.%d", i+2), Port: 41000 + i}
		in, err := g.assignClaim(uid, in)
		if err != nil {
			t.Fatal(err)
		}
		listener, err := g.handle(t.Context(), conn, uid, in)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		t.Cleanup(func() { _ = listener.Close() })
	}
	apply := func() error {
		_, err := g.handle(t.Context(), conn, uid, request{Operation: "apply"})
		return err
	}
	for i := range 20 {
		acquire(i)
	}
	// However slow one Apply is, no acquire waits for it: none ran.
	if calls != 0 {
		t.Fatalf("acquires ran %d policy transactions, want 0", calls)
	}
	if err := apply(); err != nil || calls != 1 || len(installed) != 20 {
		t.Fatalf("apply: err=%v calls=%d installed=%d, want one transaction with 20 leases", err, calls, len(installed))
	}
	if err := apply(); err != nil || calls != 1 {
		t.Fatalf("an apply with nothing new ran a transaction: err=%v calls=%d", err, calls)
	}
	acquire(20)
	failure = errors.New("WFP transaction aborted")
	if err := apply(); !errors.Is(err, failure) || len(installed) != 20 {
		t.Fatalf("failed apply: err=%v installed=%d, want the error and the previous 20-lease policy", err, len(installed))
	}
	failure = nil
	if err := apply(); err != nil || len(installed) != 21 {
		t.Fatalf("retry: err=%v installed=%d, want the 21-lease policy", err, len(installed))
	}
	if _, ok := g.leases[net.JoinHostPort("127.100.1.22", "41020")]; !ok {
		t.Fatal("the retried lease is gone")
	}
}

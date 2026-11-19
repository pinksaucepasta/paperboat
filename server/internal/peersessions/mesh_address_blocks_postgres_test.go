package peersessions

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Every fixture that seeds peer identities draws its virtual addresses from the production sequence. They used to
// come from the first byte of a hash of the fixture's timestamp: 256 blocks, so two tests of one package collided on
// peer_network_identities_virtual_address_key about once per 20 runs (main b399629fd). More than 256 accounts
// seeded one after another must all get distinct addresses.
func TestSeededMeshAccountsNeverShareVirtualAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := openMeshTestStore(t, ctx)
	prefix := fmt.Sprintf("addr_blocks_%d_", time.Now().UnixNano())
	var accounts []string
	t.Cleanup(func() {
		for _, account := range accounts {
			_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
		}
	})
	for i := range 300 {
		account := fmt.Sprintf("%s%d", prefix, i)
		accounts = append(accounts, account)
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account); err != nil {
			t.Fatal(err)
		}
		seedMeshMachines(t, ctx, store, account, 0, 0)
	}
}

package controlplane

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

func TestAdminListsUsageKeysAndSigningKeys(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	actorID, nodeID := "keylist_admin_"+suffix, "keylist_node_"+suffix
	seedRecoveryUser(t, store, actorID, "keylist_"+suffix)
	setOperatorRole(t, store, actorID, "admin")
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch) VALUES($1,'default','1.0',$2)`, nodeID, "keylist_epoch_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := NewEdgeService(store, "test")
	notBefore := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	prefix := "kl" + suffix
	for i := range 3 {
		pub := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
		pub[0] = byte(i)
		if err := service.ProvisionUsageKey(ctx, actorID, fmt.Sprintf("%s_op%d", prefix, i), fmt.Sprintf("%s_%d", prefix, i), nodeID, pub, notBefore, notBefore.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.RevokeUsageKey(ctx, actorID, prefix+"_rev", prefix+"_1", "leaked", notBefore.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	page, err := service.ListUsageKeys(ctx, actorID, prefix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].KeyID != prefix+"_0" || page.NextAfter != prefix+"_1" || page.Items[0].RevokedAt != nil || page.Items[1].RevokedAt == nil || page.Items[1].EdgeNodeID != nodeID {
		t.Fatalf("usage key page = %+v", page)
	}
	next, err := service.ListUsageKeys(ctx, actorID, page.NextAfter, 2)
	if err != nil || len(next.Items) == 0 || next.Items[0].KeyID != prefix+"_2" {
		t.Fatalf("next usage key page = %+v err=%v", next, err)
	}
	if _, err := service.ListUsageKeys(ctx, actorID, "bad cursor", 2); !errors.Is(err, ErrInvalidUsageReport) {
		t.Fatalf("bad cursor err = %v", err)
	}

	signer, err := mint.New([]mint.Key{{ID: "a-active", PrivateKey: ed25519.NewKeyFromSeed(make([]byte, 32))}, {ID: prefix + "-old", PrivateKey: ed25519.NewKeyFromSeed([]byte("keylist-old-seed-0123456789abcde"))}}, "a-active", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	service.SetSigner(signer)
	if err := service.RevokeSigningKey(ctx, actorID, prefix+"_sk", prefix+"-old", "rotated", notBefore); err != nil {
		t.Fatal(err)
	}
	keys, err := service.ListSigningKeys(ctx, actorID)
	if err != nil || keys.Total != 2 || len(keys.Items) != 2 {
		t.Fatalf("signing keys = %+v err=%v", keys, err)
	}
	if !keys.Items[0].Active || keys.Items[0].RevokedAt != nil || keys.Items[1].ID != prefix+"-old" || keys.Items[1].RevokedAt == nil || keys.Items[1].Reason != "rotated" {
		t.Fatalf("signing key states = %+v", keys.Items)
	}

	setOperatorRole(t, store, actorID, "operations")
	if _, err := service.ListUsageKeys(ctx, actorID, "", 2); !errors.Is(err, ErrAdminMutationForbidden) {
		t.Fatalf("operations usage keys err = %v", err)
	}
	if _, err := service.ListSigningKeys(ctx, actorID); !errors.Is(err, ErrAdminMutationForbidden) {
		t.Fatalf("operations signing keys err = %v", err)
	}
}

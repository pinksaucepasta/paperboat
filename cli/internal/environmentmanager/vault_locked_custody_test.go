package environmentmanager

import (
	"context"
	"errors"
	"testing"

	environmente2e "github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
)

// After a restart of the user daemon its in-memory custody is gone: opening a recording key (and every other call that
// needs the keys) must say the vault is locked, not "credential secret not found".
func TestVaultKeysNeedUnlockWhenCustodyIsGone(t *testing.T) {
	v, _, store := newVaultScopesFixture(t)
	if err := store.RemovePasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	_, err := v.RecordingKey(context.Background(), "personal:"+v.AccountID, 1, nil, nil, 1)
	if !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("RecordingKey without custody = %v, want ErrVaultLocked", err)
	}
	if err := v.WithUnlockedKeys(context.Background(), func(*environmente2e.VaultKeys) error { return nil }); !errors.Is(err, ErrVaultLocked) {
		t.Fatalf("WithUnlockedKeys without custody = %v, want ErrVaultLocked", err)
	}
}

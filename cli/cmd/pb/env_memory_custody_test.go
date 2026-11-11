package main

import "testing"

// pb env vault reset is the documented path for a user who lost both the password and the recovery code, so it must
// install its new keys into a locked memory custody; ordinary key and value commands still need an unlocked vault.
func TestVaultResetEstablishesMemoryCustody(t *testing.T) {
	for _, action := range []string{"unlock", "init", "recover", "reset"} {
		if !vaultCustodyEstablishedBy(action) {
			t.Fatalf("%s cannot install keys into a locked vault", action)
		}
	}
	for _, action := range []string{"set", "password", "rotate", "remove", "resume", "lock"} {
		if vaultCustodyEstablishedBy(action) {
			t.Fatalf("%s may install keys without unlocking", action)
		}
	}
}

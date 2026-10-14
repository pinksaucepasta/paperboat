package usermachines

import (
	"database/sql"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func TestAuthenticatedSetupBindsAliasAndDescription(t *testing.T) {
	text := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	machine := UserMachine{ID: "machine", Alias: "office", Description: "Macbook Pro 2013", PublicIdentityKey: "public", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/test"}
	input := AuthenticatedMachineInstallationInput{OperationID: "operation", InstallationGeneration: 1}
	pairing := dbsqlc.UserMachinePairing{AuthenticatedSetupCLISessionID: text("session"), AuthenticatedSetupOperationID: text("operation"), AuthenticatedSetupGeneration: sql.NullInt64{Int64: 1, Valid: true}, ApprovedByUserID: text("owner"), UserMachineID: text("machine"), PublicIdentityKey: sql.NullString{String: "public", Valid: true}, VerifierHash: []byte("hash"), RequestedAlias: sql.NullString{String: "office", Valid: true}, RequestedDescription: "Macbook Pro 2013", Platform: sql.NullString{String: "darwin", Valid: true}, Architecture: sql.NullString{String: "arm64", Valid: true}, WorkspaceRoot: sql.NullString{String: "/Users/test", Valid: true}}
	if !authenticatedMachineInstallationPairingFieldsMatch(pairing, "owner", machine, input, []byte("hash")) {
		t.Fatal("matching setup binding rejected")
	}
	pairing.RequestedDescription = "Different machine"
	if authenticatedMachineInstallationPairingFieldsMatch(pairing, "owner", machine, input, []byte("hash")) {
		t.Fatal("same verifier accepted changed description")
	}
	pairing.RequestedDescription = machine.Description
	pairing.RequestedAlias = text("other")
	if authenticatedMachineInstallationPairingFieldsMatch(pairing, "owner", machine, input, []byte("hash")) {
		t.Fatal("same verifier accepted changed alias")
	}
}

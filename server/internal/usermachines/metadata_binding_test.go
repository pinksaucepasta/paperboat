package usermachines

import (
	"database/sql"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestAuthenticatedSetupBindsAlias(t *testing.T) {
	text := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
	machine := UserMachine{ID: "machine", Alias: "office", PublicIdentityKey: "public", Platform: "darwin", Architecture: "arm64", WorkspaceRoot: "/Users/test"}
	input := AuthenticatedMachineInstallationInput{OperationID: "operation", InstallationGeneration: 1}
	pairing := dbsqlc.UserMachinePairing{AuthenticatedSetupCLISessionID: text("session"), AuthenticatedSetupOperationID: text("operation"), AuthenticatedSetupGeneration: sql.NullInt64{Int64: 1, Valid: true}, ApprovedByUserID: text("owner"), UserMachineID: text("machine"), PublicIdentityKey: sql.NullString{String: "public", Valid: true}, VerifierHash: []byte("hash"), RequestedAlias: sql.NullString{String: "office", Valid: true}, Platform: sql.NullString{String: "darwin", Valid: true}, Architecture: sql.NullString{String: "arm64", Valid: true}, WorkspaceRoot: sql.NullString{String: "/Users/test", Valid: true}}
	if !authenticatedMachineInstallationPairingFieldsMatch(pairing, "owner", machine, input, []byte("hash")) {
		t.Fatal("matching setup binding rejected")
	}
	pairing.RequestedAlias = text("other")
	if authenticatedMachineInstallationPairingFieldsMatch(pairing, "owner", machine, input, []byte("hash")) {
		t.Fatal("same verifier accepted changed alias")
	}
}

// Package teamstest sets team machine and ENV access in tests through the same policy path as the
// console's "Manage access" and `pb team machine-grant` (PLAN F18), instead of writing rows.
package teamstest

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

// SetMachineAccess sets what account (or, when account is empty, every member) may use on a
// machine bound to team; no capabilities removes the access.
func SetMachineAccess(t testing.TB, store *db.DB, team, machine, account string, capabilities ...string) {
	t.Helper()
	r := teams.MachineGrantRequest{MachineID: machine, Audience: "all_members", Capabilities: capabilities, Active: len(capabilities) > 0}
	if account != "" {
		r.Audience, r.AccountID = "selected_member", account
	}
	run(t, store, func(ctx context.Context, tx *db.Tx) error {
		return teams.ApplyMachineAccessTx(ctx, tx, config.Default().WorkspacePolicy, team, "", r)
	})
}

// SetPrivateAccess turns the team's "Allow private access" switch on or off.
func SetPrivateAccess(t testing.TB, store *db.DB, team string, on bool) {
	t.Helper()
	run(t, store, func(ctx context.Context, tx *db.Tx) error {
		return teams.SetAllowPrivateAccessTx(ctx, tx, config.Default().WorkspacePolicy, team, "", on)
	})
}

// SetENVAccess sets a member's team ENV permission (read, write, or "" for none), as
// `pb team grant <team> <account> env <team> <permission>` does.
func SetENVAccess(t testing.TB, store *db.DB, team, account, permission string) {
	t.Helper()
	run(t, store, func(ctx context.Context, tx *db.Tx) error {
		return teams.ApplyENVAccessTx(ctx, tx, config.Default().WorkspacePolicy, team, "", account, permission)
	})
}

func run(t testing.TB, store *db.DB, f func(context.Context, *db.Tx) error) {
	t.Helper()
	err := store.InTx(context.Background(), func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		return f(ctx, tx)
	})
	if err != nil {
		t.Fatal(err)
	}
}

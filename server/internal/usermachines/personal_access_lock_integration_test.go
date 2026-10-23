package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// Access to the account's own machine does not wait for the global team write lock (Lane A: concurrent exec
// setups queued ~430 ms apart behind it). While another transaction holds the lock, ten concurrent personal
// access sessions all start.
func TestPostgresPersonalAccessSessionsDoNotQueueOnTheTeamLock(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine := "pa_owner_"+suffix, "pa_machine_"+suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,ARRAY['terminal_host'],ARRAY['terminal_host'])`, machine, owner, "env_"+suffix); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	session, err := service.CreateTerminalSession(ctx, owner, machine, "personal", "op-personal")
	if err != nil {
		t.Fatal(err)
	}

	held, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	errs := make(chan error, 10)
	for i := range 10 {
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			id := fmt.Sprintf("session_pa_%s_%d", suffix, i)
			errs <- service.createAccessSession(callCtx, dbsqlc.CreateUserMachineAccessSessionParams{ID: id, UserMachineID: machine, UserID: owner, EnvironmentID: "env_" + suffix, CLIClientSessionID: "cli_" + owner, HttpBaseUrl: "https://invalid.test", HelperTerminalSessionID: "jti_" + id, ExpiresAt: time.Now().Add(time.Minute), Capabilities: []string{"exec"}, OperationID: session.ID}, machineCredentialExternal)
		}()
	}
	for range 10 {
		if err := <-errs; err != nil {
			t.Errorf("personal access session while the team lock is held: %v", err)
		}
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
}

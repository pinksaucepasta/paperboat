package teaminbox

import (
	"context"
	"errors"
	"fmt"
	serverconfig "github.com/pinksaucepasta/paperboat/server/internal/config"
	"strings"
	"testing"
	"time"
)

func TestPostgresSameMachineInboxDeliveryRequiresCurrentOwnerAndFileCapability(t *testing.T) {
	store := inboxTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	team := "self-team-" + suffix
	owner, other, machine := "self_owner_"+suffix, "self_other_"+suffix, "self_machine_"+suffix
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.teams WHERE team_id=$1`, team); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id=ANY($1::text[])`, []string{owner, other}); err != nil {
			t.Error(err)
		}
	})
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) SELECT x,x,x||'@example.test','active' FROM unnest($1::text[]) x`, []string{owner, other}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,configured_capabilities,observed_capabilities) VALUES($1,$2,$1,'linux','amd64','/workspace','online','occupied',true,ARRAY['file_receive'],ARRAY['file_receive'])`, machine, owner); err != nil {
		t.Fatal(err)
	}
	service, err := New(store, serverconfig.Default().Access.FileTransfer)
	if err != nil {
		t.Fatal(err)
	}
	input := RequestInput{RequestID: "self_request_" + suffix, OperationID: "self_operation_" + suffix, SenderAccount: owner, SourceMachineID: machine, DestinationMachineID: machine, BatchID: "self_batch_" + suffix, Files: []File{{Basename: "proof.txt", Size: 5, SHA256: strings.Repeat("a", 64)}}, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	request, err := service.Create(ctx, input)
	if err != nil || request.Status != "not_required" || request.SenderAccount != owner || request.RecipientAccount != owner || request.SourceMachineID != machine || request.DestinationMachineID != machine || request.BatchID != input.BatchID || request.RequestID != input.RequestID || request.ManifestDigest == "" {
		t.Fatalf("authorized self delivery rejected or binding changed: %v", err)
	}
	limits := serverconfig.Default().Access.FileTransfer
	limits.MaxBatchFiles = 2
	limited, err := New(store, limits)
	if err != nil {
		t.Fatal(err)
	}
	// Sizes are bounded at the descriptor by transfer mode, so the Inbox admits any size and
	// bounds only the file count.
	limitInput := input
	limitInput.Files = []File{{Basename: "first", Size: 1 << 40, SHA256: strings.Repeat("a", 64)}, {Basename: "second", Size: 5_000_000_000, SHA256: strings.Repeat("b", 64)}}
	if _, err := limited.Create(ctx, limitInput); err != nil {
		t.Fatalf("large files within the file count denied: %v", err)
	}
	limitInput.Files = nil
	for i := 0; i < 3; i++ {
		limitInput.Files = append(limitInput.Files, File{Basename: fmt.Sprintf("bounded-%d", i), Size: 1, SHA256: strings.Repeat("a", 64)})
	}
	_, err = limited.Create(ctx, limitInput)
	var bound *ManifestLimitError
	if !errors.As(err, &bound) || bound.MaxBatchFiles != limits.MaxBatchFiles {
		t.Fatal("Inbox admission ignored the configured file count or lost the typed limit")
	}
	foreign := input
	foreign.SenderAccount = other
	if _, err := service.Create(ctx, foreign); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign self delivery: %v", err)
	}
	invalid := input
	invalid.ExpiresAt = time.Now().UTC().Add(MaximumLifetime + time.Hour)
	if _, err := service.Create(ctx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatal("self delivery bypassed expiry bound")
	}
	invalid = input
	invalid.Files = []File{{Basename: "../proof.txt", Size: 5, SHA256: strings.Repeat("a", 64)}}
	if _, err := service.Create(ctx, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatal("self delivery bypassed manifest validation")
	}
	for _, column := range []string{"configured_capabilities", "observed_capabilities"} {
		if _, err := store.SQL().ExecContext(ctx, "UPDATE paperboat.user_machines SET "+column+"='{}' WHERE id=$1", machine); err != nil {
			t.Fatal(err)
		}
		if _, err := service.Create(ctx, input); !errors.Is(err, ErrOffline) {
			t.Fatalf("disabled %s accepted: %v", column, err)
		}
		if _, err := store.SQL().ExecContext(ctx, "UPDATE paperboat.user_machines SET "+column+"=ARRAY['file_receive'] WHERE id=$1", machine); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, other); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, machine, team); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("original issuer bypassed current team ownership: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=NULL WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET state='offline',online=false WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, input); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline self delivery accepted: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET revoked_at=now(),state='revoked' WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, input); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked self delivery accepted: %v", err)
	}
}

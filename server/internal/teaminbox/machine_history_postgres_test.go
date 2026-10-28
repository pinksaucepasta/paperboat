package teaminbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

func TestInboxHistoryAndPendingReceiptSurviveMachineDeletion(t *testing.T) {
	store := inboxTestStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	sender, recipient, team, source, destination, request := "history_sender_"+suffix, "history_recipient_"+suffix, "history_team_"+suffix, "history_source_"+suffix, "history_destination_"+suffix, "history_request_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{sender, recipient} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	}
	exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,'sending-host','linux','amd64','/workspace','offline','released'),($3,$4,$3,'receiving-host','linux','amd64','/workspace','offline','released')`, source, sender, destination, recipient)
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, recipient)
	exec(`INSERT INTO team_inbox_requests(request_id,operation_id,team_id,sender_account,recipient_account,source_machine_id,destination_machine_id,batch_id,manifest_digest,manifest,status,decision_kind,policy_generation,expires_at) VALUES($1,$1,$2,$3,$4,$5,$6,$1,repeat('a',64),'[{"basename":"fixture","size":1}]','completed','manual',1,now()+interval '1 hour')`, request, team, sender, recipient, source, destination)
	exec(`INSERT INTO team_inbox_receipt_outbox(request_id,recipient_account) VALUES($1,$2)`, request, recipient)
	exec(`UPDATE user_machines SET alias='renamed-host' WHERE id=$1`, source)
	exec(`DELETE FROM user_machines WHERE id IN ($1,$2)`, source, destination)
	service, err := New(store, config.Default().Access.FileTransfer)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(ctx, recipient, request)
	if err != nil || got.SourceMachineID != source || got.DestinationMachineID != destination || got.SourceMachineName != "sending-host" || got.DestinationMachineName != "receiving-host" {
		t.Fatalf("retained inbox request=%+v: %v", got, err)
	}
	page, err := service.List(ctx, recipient)
	if err != nil || len(page.Requests) != 1 || page.Requests[0].SourceMachineName != "sending-host" {
		t.Fatalf("retained inbox list=%+v: %v", page, err)
	}
	sink := &receiptSink{}
	worker, err := NewReceiptWorker(service, sink, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = service.Get(ctx, recipient, request)
	if err != nil || got.ReceiptNotification != "delivered" || len(sink.calls) != 1 {
		t.Fatalf("pending receipt after machine removal=%+v calls=%d: %v", got, len(sink.calls), err)
	}
}

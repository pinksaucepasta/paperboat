package usermachines

import (
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

func TestSelfFileTransferDescriptorRequiresCurrentPersonalOwnershipAndReadiness(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	exec := func(query string) {
		t.Helper()
		if _, err := f.store.SQL().ExecContext(ctx, query, f.machine); err != nil {
			t.Fatal("update descriptor fixture failed")
		}
	}
	refresh := func() {
		// Keep the synthetic control-plane heartbeat current during remote DB tests.
		exec(`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()
WHERE id IN(SELECT edge_node_id FROM paperboat.control_connector_generations WHERE machine_id=$1)`)
		exec(`UPDATE paperboat.runtime_carrier_attachments SET observed_at=now() WHERE machine_id=$1`)
	}
	session := "self_session_" + f.suffix
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machine_terminal_sessions
(id,user_machine_id,owner_account,name,launch_cwd) VALUES($1,$2,$3,'self-files','/workspace')`, session, f.machine, f.account); err != nil {
		t.Fatal("create session fixture failed")
	}
	for _, selectedSession := range []string{"", session} {
		refresh()
		descriptor, err := f.service.FileTransferDescriptorForRequest(ctx, f.account, f.machine, f.machine, "self-cli", selectedSession, "", "", TransferIntent{Mode: "automatic", DeclaredBytes: 1})
		if err != nil {
			t.Fatalf("authorized self descriptor rejected: %v", err)
		}
		token, _ := descriptor.Auth["token"].(string)
		claims, err := signer.VerifyCredential(token, "https://api.paperboat.test", "file_transfer", time.Now().UTC())
		if err != nil || claims.SourceMachineID != f.machine || claims.MachineID != f.machine || claims.UserID != f.account || claims.CLIClientSessionID != "self-cli" || claims.SessionID != selectedSession || claims.AssignmentID == "" || descriptor.Auth["access_session_id"] != claims.AssignmentID || descriptor.SourceMachineID != f.machine || descriptor.DestinationMachineID != f.machine || descriptor.InitiatingUserID != f.account {
			t.Fatal("self descriptor lost exact signed account, source, destination, client or session binding")
		}
	}
	for _, test := range []struct {
		name, change, restore string
		want                  error
	}{
		{"disabled receive", `UPDATE paperboat.user_machines SET configured_capabilities='{}' WHERE id=$1`, `UPDATE paperboat.user_machines SET configured_capabilities=ARRAY['terminal_host','file_receive','preview_launch'] WHERE id=$1`, ErrMachineCapabilityUnavailable},
		{"unobserved receive", `UPDATE paperboat.user_machines SET observed_capabilities=ARRAY['terminal_host'] WHERE id=$1`, `UPDATE paperboat.user_machines SET observed_capabilities=configured_capabilities WHERE id=$1`, ErrMachineOffline},
		{"offline", `UPDATE paperboat.user_machines SET online=false WHERE id=$1`, `UPDATE paperboat.user_machines SET online=true WHERE id=$1`, ErrMachineOffline},
		{"released seat", `UPDATE paperboat.user_machines SET seat_state='released' WHERE id=$1`, `UPDATE paperboat.user_machines SET seat_state='occupied' WHERE id=$1`, ErrMachineOffline},
		{"revoked", `UPDATE paperboat.user_machines SET revoked_at=now(),state='revoked' WHERE id=$1`, `UPDATE paperboat.user_machines SET revoked_at=NULL,state='online' WHERE id=$1`, ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			exec(test.change)
			defer exec(test.restore)
			for _, selectedSession := range []string{"", session} {
				refresh()
				descriptor, err := f.service.FileTransferDescriptorForRequest(ctx, f.account, f.machine, f.machine, "self-cli", selectedSession, "", "", TransferIntent{Mode: "automatic", DeclaredBytes: 1})
				if !errors.Is(err, test.want) || descriptor.Auth != nil {
					t.Fatal("self descriptor bypassed current machine authority/readiness")
				}
			}
		})
	}
	for _, actor := range []string{"foreign-account", ""} {
		if descriptor, err := f.service.FileTransferDescriptor(ctx, actor, f.machine, f.machine, "self-cli", "", TransferIntent{Mode: "automatic", DeclaredBytes: 1}); !errors.Is(err, ErrNotFound) || descriptor.Auth != nil {
			t.Fatal("foreign actor received self descriptor")
		}
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET owner_team_id=$2 WHERE id=$1`, f.machine, f.teamOne); err != nil {
		t.Fatal("transfer fixture ownership failed")
	}
	defer exec(`UPDATE paperboat.user_machines SET owner_team_id=NULL WHERE id=$1`)
	if descriptor, err := f.service.FileTransferDescriptor(ctx, f.account, f.machine, f.machine, "self-cli", "", TransferIntent{Mode: "automatic", DeclaredBytes: 1}); !errors.Is(err, ErrNotFound) || descriptor.Auth != nil {
		t.Fatal("original issuer bypassed transferred team ownership")
	}
}

func TestFileTransferModesBoundAutomaticAndSignTheModePostgres(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	ctx := workspace.WithContext(f.ctx, workspace.Scope{ID: "personal"})
	refresh := func() {
		t.Helper()
		for _, query := range []string{
			`UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now() WHERE id IN(SELECT edge_node_id FROM paperboat.control_connector_generations WHERE machine_id=$1)`,
			`UPDATE paperboat.runtime_carrier_attachments SET observed_at=now() WHERE machine_id=$1`,
		} {
			if _, err := f.store.SQL().ExecContext(ctx, query, f.machine); err != nil {
				t.Fatal("refresh descriptor fixture failed")
			}
		}
	}
	descriptor := func(intent TransferIntent) (FileTransferDescriptor, error) {
		refresh()
		return f.service.FileTransferDescriptor(ctx, f.account, f.machine, f.machine, "self-cli", "", intent)
	}
	signed := func(d FileTransferDescriptor) (string, int64) {
		t.Helper()
		token, _ := d.Auth["token"].(string)
		claims, err := signer.VerifyCredential(token, "https://api.paperboat.test", "file_transfer", time.Now().UTC())
		if err != nil {
			t.Fatalf("verify file transfer credential: %v", err)
		}
		return claims.TransferMode, claims.MaxBytes
	}

	setModes := func(on bool) {
		t.Helper()
		query := `UPDATE paperboat.user_machines SET observed_capabilities=array_remove(observed_capabilities,'file_transfer_modes') WHERE id=$1`
		if on {
			query = `UPDATE paperboat.user_machines SET observed_capabilities=array_append(array_remove(observed_capabilities,'file_transfer_modes'),'file_transfer_modes') WHERE id=$1`
		}
		if _, err := f.store.SQL().ExecContext(ctx, query, f.machine); err != nil {
			t.Fatal("set destination capability failed")
		}
	}

	// Every supported pb declares (PLAN 3b step 3): an undeclared send is refused, naming the update.
	if _, err := descriptor(TransferIntent{}); !errors.Is(err, ErrTransferIntentRequired) {
		t.Fatalf("undeclared send: %v", err)
	}

	// A destination that predates modes: a declared send within today's caps keeps today's
	// credential; Unlimited, or more than today's caps, asks for the destination's update.
	setModes(false)
	small, err := descriptor(TransferIntent{Mode: "automatic", DeclaredBytes: 500 << 20})
	if err != nil {
		t.Fatalf("declared send within today's caps to an old destination: %v", err)
	}
	if mode, max := signed(small); mode != "" || max != 0 {
		t.Fatalf("old destination got claims it refuses: mode=%q max_bytes=%d", mode, max)
	}
	for _, intent := range []TransferIntent{{Mode: "automatic", DeclaredBytes: 500<<20 + 1}, {Mode: "unlimited", DeclaredBytes: 1}} {
		_, err := descriptor(intent)
		var old *DestinationUpdateRequiredError
		if !errors.As(err, &old) {
			t.Fatalf("%+v to an old destination: %v", intent, err)
		}
	}

	// A destination that observes file_transfer_modes gets the mode and Automatic's declared total.
	setModes(true)
	at, err := descriptor(TransferIntent{Mode: "automatic", DeclaredBytes: 2_000_000_000})
	if err != nil {
		t.Fatalf("automatic at the bound: %v", err)
	}
	if mode, max := signed(at); mode != "automatic" || max != 2_000_000_000 {
		t.Fatalf("automatic claims: mode=%q max_bytes=%d", mode, max)
	}
	_, err = descriptor(TransferIntent{Mode: "automatic", DeclaredBytes: 2_000_000_001})
	var tooLarge *TransferTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.TotalBytes != 2_000_000_001 || tooLarge.MaxBytes != 2_000_000_000 {
		t.Fatalf("automatic over the bound: %v", err)
	}
	big, err := descriptor(TransferIntent{Mode: "unlimited", DeclaredBytes: 50_000_000_000})
	if err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	if mode, max := signed(big); mode != "unlimited" || max != 0 {
		t.Fatalf("unlimited claims: mode=%q max_bytes=%d", mode, max)
	}
	for _, bad := range []TransferIntent{{Mode: "", DeclaredBytes: 1}, {Mode: "fast", DeclaredBytes: 1}, {Mode: "automatic", DeclaredBytes: 0}, {Mode: "unlimited", DeclaredBytes: -1}} {
		if _, err := descriptor(bad); !errors.Is(err, ErrInvalidTransferIntent) {
			t.Fatalf("intent %+v: %v", bad, err)
		}
	}
	setModes(false)
}

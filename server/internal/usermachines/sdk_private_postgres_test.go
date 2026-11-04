package usermachines

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	"github.com/pinksaucepasta/paperboat/server/internal/nativeprivateaccess"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
)

func TestSDKSessionPrivatePortAuthorization(t *testing.T) {
	f, _, signer := newWorkspaceDescriptorFixture(t)
	ctx, token, session := sdkSession(t, f, "private_access")
	services := machineservices.New(f.store, "local.example.test")
	now := time.Now().UTC()
	err := services.RecordMachineServices(ctx, "env_"+f.suffix, f.machine, wire.Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "sdk-boot-0123456789", StartedAt: now, Generation: 1, Services: []wire.Port{{Port: 5432, AddressFamily: "ipv4"}, {Port: 5433, AddressFamily: "ipv4"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := nativeprivateaccess.NewSQLResolver(f.store)
	if err != nil {
		t.Fatal(err)
	}
	service := nativeprivateaccess.Service{Resolver: resolver, Signer: signer, Issuer: f.service.issuer}
	request := nativeprivateaccess.Request{AccountID: f.account, UserID: f.account, CLIClientSessionID: session, OperationID: "operation_sdk_private", ResourceKind: "machine_service", ResourceID: f.machine, RouteID: "tcp:5432", Protocol: "tcp"}
	grant, err := service.Issue(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := signer.VerifyCredential(grant.Credential, f.service.issuer, "native_private", time.Now())
	if err != nil || claims.CLIClientSessionID != session || claims.MachineID != f.machine || claims.RouteID != "tcp:5432" || claims.SourceMachineID != "" || claims.WorkspaceID != "personal" {
		t.Fatal("private grant lost exact session/port/workspace")
	}
	for _, tc := range []struct {
		name   string
		change func(*nativeprivateaccess.Request)
	}{
		{"other port", func(r *nativeprivateaccess.Request) { r.RouteID = "tcp:5433" }},
		{"other target", func(r *nativeprivateaccess.Request) { r.ResourceID = "foreign-machine" }},
		{"other peer", func(r *nativeprivateaccess.Request) { r.CLIClientSessionID = "foreign-session" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request
			tc.change(&r)
			if g, e := service.Issue(ctx, r); e == nil || g.Credential != "" {
				t.Fatal("unauthorized private grant issued")
			}
		})
	}
	if _, err := f.store.SQL().ExecContext(ctx, `UPDATE paperboat.api_token_scope_rules SET action='exec' WHERE token_id=$1`, token); err != nil {
		t.Fatal(err)
	}
	if g, e := service.Issue(ctx, request); e == nil || g.Credential != "" {
		t.Fatal("exec-only token obtained private access")
	}
}

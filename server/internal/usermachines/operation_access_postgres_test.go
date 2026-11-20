package usermachines

import (
	"context"
	"testing"
	"time"
)

func TestOperationDescriptorsReuseLiveAuthorityPostgres(t *testing.T) {
	f, source, signer := newWorkspaceDescriptorFixture(t)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines
SET configured_capabilities=array_append(configured_capabilities,'ssh_host'),
    observed_capabilities=array_append(observed_capabilities,'ssh_host') WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind  operationKind
		issue func(context.Context, string, string, string, string, string, bool) (ExecDescriptor, error)
	}{{execOperation, f.service.ExecDescriptor}, {sshOperation, f.service.SSHDescriptor}} {
		t.Run(tc.kind.capability, func(t *testing.T) {
			const operationID = "operation_repeat"
			client := "cli_repeat_" + f.suffix
			type result struct {
				d   ExecDescriptor
				err error
			}
			results := make(chan result, 2)
			for range 2 {
				go func() {
					d, err := tc.issue(f.ctx, f.account, source, f.machine, client, operationID, true)
					results <- result{d, err}
				}()
			}
			first, second := <-results, <-results
			if first.err != nil || second.err != nil {
				t.Fatalf("issue errors %v, %v", first.err, second.err)
			}
			if first.d.Auth.AccessSessionID == "" || first.d.Auth.AccessSessionID != second.d.Auth.AccessSessionID || !first.d.ExpiresAt.Equal(second.d.ExpiresAt) {
				t.Fatal("simultaneous retries issued separate access sessions or deadlines")
			}
			jti := func(d ExecDescriptor) string {
				t.Helper()
				claims, err := signer.VerifyCredential(d.Auth.Token, f.service.issuer, tc.kind.class, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				return claims.JTI
			}
			if jti(first.d) != jti(second.d) {
				t.Fatal("retry changed the revocation identity")
			}
			fromOtherSource, err := tc.issue(f.ctx, f.account, f.machine, f.machine, client, operationID, true)
			if err != nil || fromOtherSource.Auth.AccessSessionID == first.d.Auth.AccessSessionID {
				t.Fatalf("different source did not get distinct authority: %v", err)
			}
		})
	}
}

func TestReleaseOperationAccessRevokesRenewalsForTheExactCallerPostgres(t *testing.T) {
	f, source, _ := newWorkspaceDescriptorFixture(t)
	if _, err := f.store.SQL().ExecContext(f.ctx, `UPDATE paperboat.user_machines
SET configured_capabilities=array_append(configured_capabilities,'ssh_host'),
    observed_capabilities=array_append(observed_capabilities,'ssh_host') WHERE id=$1`, f.machine); err != nil {
		t.Fatal(err)
	}
	const op = "operation_release"
	client := "cli_release_" + f.suffix
	issue := func(kind operationKind, peer string) ExecDescriptor {
		t.Helper()
		d, err := f.service.operationDescriptor(f.ctx, kind, f.account, source, f.machine, peer, op, true)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	exec := issue(execOperation, client)
	ssh := issue(sshOperation, client)
	other := issue(execOperation, "another_"+client)
	renewAt := exec.ExpiresAt.Add(time.Second)
	f.service.now = func() time.Time { return renewAt }
	renewal := issue(execOperation, client)
	if renewal.Auth.AccessSessionID == exec.Auth.AccessSessionID || !renewal.ExpiresAt.After(renewAt) {
		t.Fatal("expired exec credential was not replaced")
	}
	release := func(account, cli, kind string) {
		t.Helper()
		if err := f.service.ReleaseOperationAccess(f.ctx, account, cli, f.machine, op, kind); err != nil {
			t.Fatal(err)
		}
	}
	state := func(d ExecDescriptor, want string) {
		t.Helper()
		var got string
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT state FROM paperboat.user_machine_access_sessions WHERE id=$1`, d.Auth.AccessSessionID).Scan(&got); err != nil || got != want {
			t.Fatalf("access session state=%q want=%q err=%v", got, want, err)
		}
	}
	release("foreign_"+f.account, client, execOperation.capability)
	state(exec, "active")
	state(renewal, "active")
	for range 2 {
		release(f.account, client, execOperation.capability)
	}
	state(exec, "revoked")
	state(renewal, "revoked")
	state(other, "active")
	state(ssh, "active")
	release(f.account, client, sshOperation.capability)
	state(ssh, "revoked")
}

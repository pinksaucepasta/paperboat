package usermachines

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTerminalJoinCodeNormalizationAndResourceBinding(t *testing.T) {
	code, err := newTerminalJoinCode()
	if err != nil {
		t.Fatal(err)
	}
	normalized, valid := normalizeTerminalJoinCode(strings.ToLower(code))
	if !valid || len(normalized) != 12 {
		t.Fatal("generated code could not be normalized")
	}
	if terminalJoinCodeHash("one", code) != terminalJoinCodeHash("one", normalized) {
		t.Fatal("formatted code hashes differ")
	}
	if terminalJoinCodeHash("one", code) == terminalJoinCodeHash("two", code) {
		t.Fatal("code hash not terminal bound")
	}
	for _, bad := range []string{"", "123456", "AAAA-AAAA-AAA!", "OOOO-OOOO-OOOO", code + "X"} {
		if _, ok := normalizeTerminalJoinCode(bad); ok {
			t.Fatalf("accepted malformed code %q", bad)
		}
	}
}

func TestTerminalCodeAccessLifecycleAndBoundsPostgres(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, outsider, machine, session, other := "code_owner_"+suffix, "code_other_"+suffix, "code_machine_"+suffix, "code_terminal_"+suffix, "code_second_"+suffix
	insert := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	insert(`INSERT INTO users(id,workos_subject,primary_email,email_verified,status) VALUES($1,$1,'owner@company.test',true,'active'),($2,$2,'other@outside.test',true,'active')`, owner, outsider)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		store.SQL().ExecContext(cleanup, `DELETE FROM user_machine_terminal_sessions WHERE id IN ($1,$2)`, session, other)
		store.SQL().ExecContext(cleanup, `DELETE FROM user_machines WHERE id=$1`, machine)
		store.SQL().ExecContext(cleanup, `DELETE FROM users WHERE id IN ($1,$2)`, owner, outsider)
	})
	insert(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$3,'code-host','linux','amd64','/workspace','online','occupied')`, machine, owner, "env_code_"+suffix)
	insert(`INSERT INTO user_machine_terminal_sessions(id,user_machine_id,name,launch_cwd,owner_account,desired_state,runtime_state) VALUES($1,$2,'test','/workspace',$3,'open','running'),($4,$2,'other','/workspace',$3,'open','running')`, session, machine, owner, other)
	service := New(store, nil, Policy{}, nil)
	state, err := service.TerminalCodeAccess(ctx, owner, session)
	if err != nil || state.Enabled || state.Generation != 0 || state.Role != TerminalBrowserRoleViewer {
		t.Fatalf("default state %+v: %v", state, err)
	}
	if _, err = service.TerminalCodeAccess(ctx, outsider, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("nonowner read %v", err)
	}
	in := TerminalCodeAccessMutation{Enabled: true, Role: TerminalBrowserRoleViewer, ExpiresInSeconds: 3600}
	if _, err = service.SetTerminalCodeAccess(ctx, outsider, session, in); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("nonowner activation %v", err)
	}
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, in)
	if err != nil || !state.Enabled || state.Generation != 1 || state.Code == "" {
		t.Fatalf("activate %+v: %v", state, err)
	}
	originalCode := state.Code
	read, err := service.TerminalCodeAccess(ctx, owner, session)
	if err != nil || read.Code != "" {
		t.Fatalf("read leaks stored code %+v: %v", read, err)
	}
	if _, err = service.JoinTerminalWithCode(ctx, session, "AAAA-AAAA-AAAA", "Guest"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("wrong code %v", err)
	}
	if _, err = service.JoinTerminalWithCode(ctx, other, originalCode, "Guest"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("cross terminal code %v", err)
	}
	if _, err = service.JoinTerminalWithCode(ctx, session, originalCode, "\x1b[31m"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("control name %v", err)
	}
	guest, err := service.JoinTerminalWithCode(ctx, session, strings.ToLower(originalCode), "Guest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.AuthenticateTerminalGuest(ctx, other, guest.Token); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("cross terminal bearer %v", err)
	}
	if _, err = service.AuthenticateTerminalGuest(ctx, session, guest.Token); err != nil {
		t.Fatal(err)
	}
	decision, err := service.ResolveTerminalGuestRole(ctx, guest.ID, session)
	if err != nil || decision.Role != TerminalBrowserRoleViewer || decision.GuestSessionID != guest.ID || decision.AccountID != "" || decision.WorkspaceID != "personal" {
		t.Fatalf("guest viewer %+v: %v", decision, err)
	}
	metadata, err := service.TerminalBrowserSession(ctx, "", guest.ID, session)
	if err != nil || metadata.CanManage || metadata.OwnerAccount != owner || metadata.Role != "viewer" {
		t.Fatalf("guest metadata %+v: %v", metadata, err)
	}
	observed, err := service.TerminalCodeAccess(ctx, owner, session)
	if err != nil || len(observed.Guests) != 1 || observed.Guests[0].DisplayName != "Guest" || observed.Guests[0].ID != guest.ID {
		t.Fatalf("owner guest sessions=%+v err=%v", observed.Guests, err)
	}

	if _, err = service.ResolveTerminalBrowserRole(ctx, outsider, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("outsider gained account grant %v", err)
	}
	// A new guest must not delete a still-admitted guest whose bearer expired.
	insert(`UPDATE terminal_guest_sessions SET created_at=now()-interval '1 minute',expires_at=now()-interval '1 second' WHERE id=$1`, guest.ID)
	if _, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Next guest"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session); err != nil {
		t.Fatalf("joining guest removed admitted authority: %v", err)
	}
	// Changing account policy must not revoke a separately admitted guest.
	policy, err := service.SetTerminalBrowserAccessPolicy(ctx, owner, session, TerminalBrowserAccessPolicyMutation{ExpectedGeneration: 1, Audience: "specific_emails", AllowedEmails: []string{"other@outside.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session); err != nil {
		t.Fatalf("account change revoked guest %v", err)
	}
	account, err := service.ResolveTerminalBrowserRole(ctx, outsider, session)
	if err != nil {
		t.Fatal(err)
	}
	in.ExpectedGeneration = 1
	in.Reset = true
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, in)
	if err != nil || state.Generation != 2 || state.Code == originalCode {
		t.Fatalf("rotation %+v: %v", state, err)
	}
	if _, err = service.AuthenticateTerminalGuest(ctx, session, guest.Token); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("reset bearer %v", err)
	}
	if _, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("reset authority %v", err)
	}
	if _, err = service.JoinTerminalWithCode(ctx, session, originalCode, "Guest"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("reset code %v", err)
	}
	latest, err := service.ResolveTerminalBrowserRole(ctx, outsider, session)
	if err != nil || !sameTerminalBrowserDecision(account, latest) || latest.PolicyGeneration != policy.Generation {
		t.Fatalf("rotation disturbed account %+v: %v", latest, err)
	}
	// Control is a separate explicit grant; a guest can never become owner.
	in.ExpectedGeneration = 2
	in.Role = TerminalBrowserRoleInteractive
	in.ConfirmInteractiveAccess = false
	if _, err = service.SetTerminalCodeAccess(ctx, owner, session, in); !errors.Is(err, ErrTerminalBrowserInteractionConfirmationRequired) {
		t.Fatalf("unconfirmed control %v", err)
	}
	in.ConfirmInteractiveAccess = true
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, in)
	if err != nil {
		t.Fatal(err)
	}
	guest, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Control guest")
	if err != nil {
		t.Fatal(err)
	}
	decision, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session)
	if err != nil || decision.Role != TerminalBrowserRoleInteractive {
		t.Fatalf("control %+v: %v", decision, err)
	}
	stop := TerminalCodeAccessMutation{ExpectedGeneration: state.Generation, Role: TerminalBrowserRoleViewer}
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, stop)
	if err != nil || state.Enabled {
		t.Fatalf("stop %+v: %v", state, err)
	}
	if _, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session); !errors.Is(err, ErrTerminalSessionNotFound) {
		t.Fatalf("stop authority %v", err)
	}
	// Expiry denies new bearer/code admission, while an admitted guest retains authority.
	in.ExpectedGeneration = state.Generation
	in.Role = TerminalBrowserRoleViewer
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, in)
	if err != nil {
		t.Fatal(err)
	}
	guest, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Expiry guest")
	if err != nil {
		t.Fatal(err)
	}
	insert(`UPDATE terminal_code_access SET expires_at=now()-interval '1 second' WHERE terminal_session_id=$1`, session)
	if _, err = service.ResolveTerminalGuestRole(ctx, guest.ID, session); err != nil {
		t.Fatalf("admitted guest lost authority at credential expiry: %v", err)
	}
	if _, err = service.AuthenticateTerminalGuest(ctx, session, guest.Token); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("expired bearer %v", err)
	}
	if _, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Guest"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("expired code %v", err)
	}
	read, err = service.TerminalCodeAccess(ctx, owner, session)
	if err != nil || read.Enabled {
		t.Fatalf("expired display %+v: %v", read, err)
	}
	// Reactivation rotates and cleans old rows, then concurrent joins enforce a strict cap.
	in.ExpectedGeneration = state.Generation
	state, err = service.SetTerminalCodeAccess(ctx, owner, session, in)
	if err != nil {
		t.Fatal(err)
	}
	insert(`INSERT INTO terminal_guest_sessions(id,token_hash,terminal_session_id,code_generation,display_name,expires_at)
 SELECT $1||'-capacity-'||i,decode(md5($1||'-capacity-'||i)||md5($1||'-hash-'||i),'hex'),$1,$2,'Bounded guest',$3
 FROM generate_series(1,$4) i`, session, state.Generation, state.ExpiresAt, planlimits.ViewersPerSharedTerminal()-1)

	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := service.JoinTerminalWithCode(ctx, session, state.Code, "Concurrent guest")
			results <- err
		}()
	}
	group.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrTerminalGuestCapacity) {
			t.Fatalf("concurrent join %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("concurrent admissions %d, want 1", accepted)
	}
	insert(`UPDATE terminal_code_access SET attempts=120,attempts_window=now() WHERE terminal_session_id=$1`, session)
	if _, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Guest"); !errors.Is(err, ErrTerminalCodeRateLimited) {
		t.Fatalf("rate limit %v", err)
	}
	insert(`UPDATE terminal_code_access SET attempts_window=now()-interval '2 minutes' WHERE terminal_session_id=$1`, session)
	if _, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Guest"); !errors.Is(err, ErrTerminalGuestCapacity) {
		t.Fatalf("rate window recovery %v", err)
	}
	insert(`UPDATE user_machine_terminal_sessions SET desired_state='closed',runtime_state='closed' WHERE id=$1`, session)
	if _, err = service.JoinTerminalWithCode(ctx, session, state.Code, "Guest"); !errors.Is(err, ErrTerminalCodeInvalid) {
		t.Fatalf("closed join %v", err)
	}
}

package recording

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func key(t *testing.T, gen int64, signer ed25519.PrivateKey, team string) (Key, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k := Key{Generation: gen, PublicKey: make([]byte, 32), SigningPublicKey: pub}
	rand.Read(k.PublicKey)
	if signer != nil {
		k.RotationSignature = ed25519.Sign(signer, RotationMessage(team, gen, k.PublicKey, k.SigningPublicKey, k.ResetPublicKey))
	}
	return k, priv
}

func TestRotationAcceptsOnlyASuccessorSignedByThePreviousSigningKey(t *testing.T) {
	g1, s1 := key(t, 1, nil, "team_a")
	g2, s2 := key(t, 2, s1, "team_a")
	if !VerifyRotation("team_a", g1, g2) {
		t.Fatal("valid rotation refused")
	}
	if VerifyRotation("team_b", g1, g2) {
		t.Fatal("rotation accepted for another team")
	}
	forged, _ := key(t, 2, s2, "team_a") // signed by the new key, not the pinned one
	if VerifyRotation("team_a", g1, forged) {
		t.Fatal("rotation not signed by the pinned key accepted")
	}
	g3, _ := key(t, 3, s1, "team_a")
	if VerifyRotation("team_a", g1, g3) {
		t.Fatal("generation skip accepted")
	}
	g2.PublicKey[0] ^= 1
	if VerifyRotation("team_a", g1, g2) {
		t.Fatal("changed public key accepted")
	}
	if (Key{Generation: 1, PublicKey: make([]byte, 32), SigningPublicKey: make([]byte, 32), RotationSignature: make([]byte, 64)}).ValidKey() {
		t.Fatal("generation 1 with a rotation signature accepted")
	}
}

// A reset generation is accepted only on a signature by the reset key the previous generation carries (the owner's ENV
// writer key), never on the signature of the new key itself or of a key nobody pinned; a rotation signature is not a
// reset signature and the other way round.
func TestResetAcceptsOnlyASuccessorSignedByThePinnedResetKey(t *testing.T) {
	ownerPub, owner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	g1, _ := key(t, 1, nil, "team_a")
	g1.ResetPublicKey = ownerPub
	next := func(signer ed25519.PrivateKey, gen int64) Key {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		k := Key{Generation: gen, PublicKey: make([]byte, 32), SigningPublicKey: pub, ResetPublicKey: ownerPub}
		rand.Read(k.PublicKey)
		k.ResetSignature = ed25519.Sign(signer, ResetMessage("team_a", gen, k.PublicKey, k.SigningPublicKey, k.ResetPublicKey))
		return k
	}
	g2 := next(owner, 2)
	if !g2.ValidKey() || !VerifyReset("team_a", g1, g2) || !VerifyLink("team_a", g1, g2) || VerifyRotation("team_a", g1, g2) {
		t.Fatal("a reset signed by the pinned reset key was refused")
	}
	if VerifyReset("team_b", g1, g2) {
		t.Fatal("reset accepted for another team")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyLink("team_a", g1, next(other, 2)) {
		t.Fatal("a reset signed by a key nobody pinned was accepted")
	}
	if VerifyLink("team_a", g1, next(owner, 3)) {
		t.Fatal("a generation skip was accepted")
	}
	noReset, _ := key(t, 1, nil, "team_a")
	if VerifyReset("team_a", noReset, g2) {
		t.Fatal("a key without a reset key was reset")
	}
	both := g2
	both.RotationSignature = make([]byte, 64)
	if both.ValidKey() {
		t.Fatal("a generation with both a rotation and a reset signature is valid")
	}
	// A rotation signature binds the reset key it carries: changing it breaks the signature.
	r2, s := key(t, 1, nil, "team_a")
	r3, _ := key(t, 2, s, "team_a")
	r3.ResetPublicKey = ownerPub
	if VerifyRotation("team_a", r2, r3) {
		t.Fatal("a rotation still verified after the reset key it carries was changed")
	}
}

func TestReportValidatesPerKind(t *testing.T) {
	now := time.Now()
	id := "rec_01234567-89ab-cdef-0123-456789abcdef"
	ok := []struct {
		kind string
		r    Report
	}{
		{KindStarted, Report{RecordingID: id, SessionID: "sess_1", Trigger: TriggerRule, KeyGeneration: 1, StartedAt: now, Accounts: []string{"user_1"}}},
		{KindStarted, Report{RecordingID: id, SessionID: "sess_1", Trigger: TriggerManual, ActorAccountID: "user_1", KeyGeneration: 1, StartedAt: now}},
		{KindStopped, Report{RecordingID: id, Reason: ReasonStopped, ActorAccountID: "user_1", EndedAt: now, Bytes: 10, Segments: 1}},
		{KindStopped, Report{RecordingID: id, Reason: ReasonSessionEnded, EndedAt: now}},
		{KindDeleted, Report{RecordingID: id, Reason: ReasonRetention}},
	}
	for i, c := range ok {
		if err := c.r.Validate(c.kind, now); err != nil {
			t.Errorf("case %d: %v", i, err)
		}
	}
	bad := []struct {
		kind string
		r    Report
	}{
		{KindStarted, Report{RecordingID: "rec_x", SessionID: "s", Trigger: TriggerRule, KeyGeneration: 1, StartedAt: now}},
		{KindStarted, Report{RecordingID: id, SessionID: "s", Trigger: TriggerManual, KeyGeneration: 1, StartedAt: now}},
		{KindStarted, Report{RecordingID: id, SessionID: "s", Trigger: TriggerRule, KeyGeneration: 0, StartedAt: now}},
		{KindStarted, Report{RecordingID: id, SessionID: "s", Trigger: TriggerRule, KeyGeneration: 1, StartedAt: now.Add(-8 * 24 * time.Hour)}},
		{KindStopped, Report{RecordingID: id, Reason: ReasonStopped, EndedAt: now}},
		{KindStopped, Report{RecordingID: id, Reason: "other", EndedAt: now}},
		{KindDeleted, Report{RecordingID: id, Reason: ReasonError}},
		{KindStarted, Report{RecordingID: id, SessionID: "s", Trigger: TriggerRule, KeyGeneration: 1, StartedAt: now, Accounts: make([]string, MaxAccounts+1)}},
		{"recording_other", Report{RecordingID: id}},
	}
	for i, c := range bad {
		if c.r.Validate(c.kind, now) == nil {
			t.Errorf("bad case %d accepted", i)
		}
	}
}

func TestStateAndPolicyValidate(t *testing.T) {
	now := time.Now()
	id := "rec_01234567-89ab-cdef-0123-456789abcdef"
	if !(State{ID: id, Trigger: TriggerRule, StartedAt: now}).Valid() || !(State{ID: id, Trigger: TriggerManual, StartedBy: "user_1", StartedAt: now}).Valid() {
		t.Fatal("valid state refused")
	}
	for _, s := range []State{{ID: id, Trigger: TriggerManual, StartedAt: now}, {ID: id, Trigger: TriggerRule, StartedBy: "user_1", StartedAt: now}, {ID: id, Trigger: TriggerRule}, {ID: "x", Trigger: TriggerRule, StartedAt: now}} {
		if s.Valid() {
			t.Errorf("invalid state accepted: %+v", s)
		}
	}
	k1, s1 := key(t, 1, nil, "team_a")
	k2, _ := key(t, 2, s1, "team_a")
	p := Policy{Enabled: true, TeamID: "team_a", Keys: []Key{k1, k2}, ManualStart: ManualStartAdmins, RetentionDays: 30, DiskQuotaMB: 2048}
	if !p.Valid() {
		t.Fatal("valid policy refused")
	}
	for name, bad := range map[string]func(Policy) Policy{
		"manual start": func(p Policy) Policy { p.ManualStart = "everyone"; return p },
		"retention":    func(p Policy) Policy { p.RetentionDays = 0; return p },
		"key gap":      func(p Policy) Policy { p.Keys = []Key{k2}; return p },
		"no team":      func(p Policy) Policy { p.TeamID = ""; return p },
	} {
		if bad(p).Valid() {
			t.Errorf("%s accepted", name)
		}
	}
	if ValidSealed(nil) || ValidSealed(make([]byte, SealedBytes+1)) || !ValidSealed(make([]byte, SealedBytes)) {
		t.Fatal("ValidSealed bounds")
	}
}

func TestPersonalScope(t *testing.T) {
	scope := PersonalScope("user_1")
	if scope != "personal:user_1" || !IsPersonalScope(scope) || IsPersonalScope("personal") || IsPersonalScope("acme") {
		t.Fatalf("scope %q", scope)
	}
	if WorkspaceOf(scope) != PersonalWorkspace || WorkspaceOf("acme") != "acme" || !idPattern.MatchString(scope) {
		t.Fatal("WorkspaceOf or the id pattern does not accept a personal scope")
	}
}

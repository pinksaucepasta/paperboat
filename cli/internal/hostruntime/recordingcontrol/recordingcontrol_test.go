package recordingcontrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

type keyPair struct {
	key     wire.Key
	signing ed25519.PrivateKey
}

func newKey(t *testing.T, team string, generation int64, previous *keyPair) keyPair {
	t.Helper()
	public := make([]byte, 32)
	_, _ = rand.Read(public)
	signingPublic, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key := wire.Key{Generation: generation, PublicKey: public, SigningPublicKey: signingPublic}
	if previous != nil {
		key.RotationSignature = ed25519.Sign(previous.signing, wire.RotationMessage(team, generation, public, signingPublic))
	}
	return keyPair{key: key, signing: signing}
}

func policyWith(version int64, keys ...wire.Key) wire.Policy {
	return wire.Policy{Version: version, Enabled: true, TeamID: "team_1", Keys: keys, ManualStart: wire.ManualStartAnyone, AdminAccounts: []string{"admin"}, RecordAccounts: []string{"watched"}, RetentionDays: 30, DiskQuotaMB: 64}
}

func TestPolicyPinsTheFirstKeyAndFollowsOnlySignedRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	store, err := OpenPolicyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first := newKey(t, "team_1", 1, nil)
	second := newKey(t, "team_1", 2, &first)
	if got, err := store.Replace(policyWith(1, first.key, second.key)); err != nil || got.Key == nil || got.Key.Generation != 2 || !got.Usable() {
		t.Fatalf("first use %+v %v", got.Key, err)
	}
	// A rotation signed by someone else (the server) is not followed.
	forged := newKey(t, "team_1", 3, &keyPair{signing: newKey(t, "team_1", 1, nil).signing})
	if got, err := store.Replace(policyWith(2, first.key, second.key, forged.key)); !errors.Is(err, ErrKeyUnverified) || got.Key != nil || got.Usable() {
		t.Fatalf("forged rotation %+v %v", got.Key, err)
	}
	// Swapping the pinned first key is refused, also after a restart.
	other := newKey(t, "team_1", 1, nil)
	reopened, err := OpenPolicyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Replace(policyWith(3, other.key)); !errors.Is(err, ErrKeyUnverified) || got.Key != nil {
		t.Fatalf("swapped pin %+v %v", got.Key, err)
	}
	third := newKey(t, "team_1", 3, &second)
	if got, err := reopened.Replace(policyWith(4, first.key, second.key, third.key)); err != nil || got.Key == nil || got.Key.Generation != 3 {
		t.Fatalf("signed rotation %+v %v", got.Key, err)
	}
	// The server's version is a content digest: a changed policy replaces the cached one even when its number is lower.
	if got, _ := reopened.Replace(policyWith(1, first.key, second.key, third.key)); got.Version != 1 || got.Key == nil || got.Key.Generation != 3 {
		t.Fatalf("a changed policy with a lower version was ignored: %+v", got.Version)
	}
}

type fakeSessions struct {
	snapshots map[string]session.Snapshot
	metas     []recordings.Meta
	started   []recordings.StartRequest
	stopped   []string
	pruned    []recordings.Meta
}

func (f *fakeSessions) List() []session.Snapshot {
	var out []session.Snapshot
	for id, snapshot := range f.snapshots {
		snapshot.ID = id
		out = append(out, snapshot)
	}
	return out
}
func (f *fakeSessions) Snapshot(id string) (session.Snapshot, error) {
	snapshot, ok := f.snapshots[id]
	if !ok {
		return session.Snapshot{}, session.ErrSessionUnknown
	}
	return snapshot, nil
}
func (f *fakeSessions) StartRecording(id string, request recordings.StartRequest) (recordings.State, error) {
	snapshot := f.snapshots[id]
	if snapshot.Recording != nil {
		return recordings.State{}, recordings.ErrRecording
	}
	state := recordings.State{ID: "rec_" + id, Trigger: request.Trigger, StartedBy: request.ActorAccountID}
	snapshot.Recording = &state
	f.snapshots[id] = snapshot
	f.started = append(f.started, request)
	return state, nil
}
func (f *fakeSessions) StopRecording(id, actor, reason string) (recordings.Meta, error) {
	snapshot := f.snapshots[id]
	snapshot.Recording = nil
	f.snapshots[id] = snapshot
	f.stopped = append(f.stopped, id+":"+reason)
	return recordings.Meta{}, nil
}
func (f *fakeSessions) Recordings() ([]recordings.Meta, error) { return f.metas, nil }
func (f *fakeSessions) RecordingSegment(id string, index uint32) ([]byte, error) {
	if id == "rec_pruned" {
		return nil, recordings.ErrNotFound
	}
	return []byte(fmt.Sprintf("segment %s %d", id, index)), nil
}
func (f *fakeSessions) PruneRecordings(time.Duration, int64) ([]recordings.Meta, error) {
	return f.pruned, nil
}
func (f *fakeSessions) CloseInterruptedRecording(id string) (recordings.Meta, error) {
	for i := range f.metas {
		if f.metas[i].ID == id {
			now := time.Now()
			f.metas[i].EndedAt, f.metas[i].EndReason = &now, recordings.ReasonError
			return f.metas[i], nil
		}
	}
	return recordings.Meta{}, recordings.ErrNotFound
}

func testController(t *testing.T, policy wire.Policy, sessions *fakeSessions) (*Controller, *offlineaudit.Queue) {
	t.Helper()
	root := t.TempDir()
	store, err := OpenPolicyStore(filepath.Join(root, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(policy); err != nil {
		t.Fatal(err)
	}
	queue, err := offlineaudit.Open(filepath.Join(root, "audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(Config{Sessions: sessions, Policies: store, Audit: queue, LedgerPath: filepath.Join(root, "ledger.json")})
	if err != nil {
		t.Fatal(err)
	}
	return controller, queue
}

func running(participants ...string) session.Snapshot {
	snapshot := session.Snapshot{State: session.Running}
	for _, account := range participants {
		snapshot.Participants = append(snapshot.Participants, session.Participant{AccountID: account})
	}
	return snapshot
}

func TestWhoMayStartAndStop(t *testing.T) {
	first := newKey(t, "team_1", 1, nil)
	sessions := &fakeSessions{snapshots: map[string]session.Snapshot{"s1": running("dev"), "s2": running("watched")}}
	policy := policyWith(1, first.key)
	controller, _ := testController(t, policy, sessions)
	state, err := controller.StartRecording("s1", "dev")
	if err != nil || state.Trigger != recordings.TriggerManual || sessions.started[0].QuotaBytes != 64<<20 || sessions.started[0].Key.Generation != 1 {
		t.Fatalf("manual start %+v %v", state, err)
	}
	if _, err := controller.StopRecording("s1", "someone_else"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger stopped a manual recording: %v", err)
	}
	if _, err := controller.StopRecording("s1", "dev"); err != nil {
		t.Fatalf("the starter could not stop: %v", err)
	}
	if !controller.Attached("s2", "watched") || controller.Attached("s1", "dev") {
		t.Fatal("rule recordings start only for recorded accounts")
	}
	if _, err := controller.StopRecording("s2", "watched"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a participant stopped a rule recording: %v", err)
	}
	if _, err := controller.StopRecording("s2", "admin"); err != nil {
		t.Fatalf("an admin could not stop a rule recording: %v", err)
	}
	policy.Version, policy.ManualStart = 2, wire.ManualStartAdmins
	if _, err := controller.config.Policies.Replace(policy); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.StartRecording("s1", "dev"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admins-only start allowed a member: %v", err)
	}
	if _, err := controller.StartRecording("s1", "admin"); err != nil {
		t.Fatal(err)
	}
	policy.Version, policy.Enabled = 3, false
	got, _ := controller.config.Policies.Replace(policy)
	controller.reconcile(got)
	if sessions.snapshots["s1"].Recording != nil || sessions.stopped[len(sessions.stopped)-1] != "s1:"+recordings.ReasonPolicy {
		t.Fatalf("a team that stopped recording did not stop the recordings: %v", sessions.stopped)
	}
	if _, err := controller.StartRecording("s1", "admin"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("start without the plan: %v", err)
	}
}

func TestRemovingARuleStopsTheRecordingsItStarted(t *testing.T) {
	first := newKey(t, "team_1", 1, nil)
	sessions := &fakeSessions{snapshots: map[string]session.Snapshot{"watched": running("watched"), "manual": running("dev"), "left": running(), "both": running("watched", "other")}}
	policy := policyWith(1, first.key)
	policy.RecordAccounts = []string{"watched", "other"}
	controller, _ := testController(t, policy, sessions)
	if !controller.Attached("watched", "watched") || !controller.Attached("left", "watched") || !controller.Attached("both", "other") {
		t.Fatal("the rule did not start its recordings")
	}
	if _, err := controller.StartRecording("manual", "dev"); err != nil {
		t.Fatal(err)
	}
	// "watched" is no longer recorded: its session stops; "both" still holds a recorded account; "left" has nobody attached
	// and the rule still records someone, so it goes on; a manual recording is nobody's rule.
	policy.Version, policy.RecordAccounts = 2, []string{"other"}
	got, _ := controller.config.Policies.Replace(policy)
	controller.reconcile(got)
	if sessions.snapshots["watched"].Recording != nil || sessions.snapshots["both"].Recording == nil || sessions.snapshots["left"].Recording == nil || sessions.snapshots["manual"].Recording == nil {
		t.Fatalf("a rule change stopped the wrong recordings: %v", sessions.stopped)
	}
	// The last rule removed stops every rule recording and still leaves the manual one.
	policy.Version, policy.RecordAccounts = 3, nil
	got, _ = controller.config.Policies.Replace(policy)
	controller.reconcile(got)
	if sessions.snapshots["both"].Recording != nil || sessions.snapshots["left"].Recording != nil || sessions.snapshots["manual"].Recording == nil {
		t.Fatalf("removing the last rule left rule recordings running: %v", sessions.stopped)
	}
	for _, stop := range sessions.stopped {
		if !strings.HasSuffix(stop, ":"+recordings.ReasonPolicy) {
			t.Fatalf("a rule recording ended for another reason: %v", sessions.stopped)
		}
	}
}

func TestAddingARuleRecordsTheAccountsAlreadyAttached(t *testing.T) {
	first := newKey(t, "team_1", 1, nil)
	sessions := &fakeSessions{snapshots: map[string]session.Snapshot{"late": running("late"), "other": running("dev")}}
	policy := policyWith(1, first.key)
	controller, _ := testController(t, policy, sessions)
	policy.Version, policy.RecordAccounts = 2, []string{"watched", "late"}
	got, _ := controller.config.Policies.Replace(policy)
	controller.reconcile(got)
	if rec := sessions.snapshots["late"].Recording; rec == nil || rec.Trigger != recordings.TriggerRule || sessions.snapshots["other"].Recording != nil {
		t.Fatalf("a new rule did not start recording the attached account only: %+v", sessions.started)
	}
}

func TestReportsReachTheAuditQueueOnceEach(t *testing.T) {
	first := newKey(t, "team_1", 1, nil)
	now := time.Now().UTC()
	ended := now
	sessions := &fakeSessions{snapshots: map[string]session.Snapshot{"live": running("dev")}, metas: []recordings.Meta{
		{ID: "rec_00000000-0000-4000-8000-000000000001", SessionID: "live", Trigger: recordings.TriggerManual, StartedBy: "dev", Accounts: []string{"dev"}, KeyGeneration: 1, StartedAt: now},
		{ID: "rec_00000000-0000-4000-8000-000000000002", SessionID: "gone", Trigger: recordings.TriggerRule, KeyGeneration: 1, StartedAt: now},
		{ID: "rec_00000000-0000-4000-8000-000000000003", SessionID: "done", Trigger: recordings.TriggerRule, KeyGeneration: 1, StartedAt: now, EndedAt: &ended, EndReason: recordings.ReasonStopped, StoppedBy: "admin", Bytes: 10, Segments: 1},
	}}
	controller, queue := testController(t, policyWith(1, first.key), sessions)
	controller.report()
	kinds := map[string]int{}
	for _, event := range queue.Oldest(100) {
		if event.Recording == nil || event.Recording.Validate(event.Kind, time.Now()) != nil || event.EventID != eventID(event.Recording.RecordingID, event.Kind) {
			t.Fatalf("bad event %+v", event)
		}
		kinds[event.Kind]++
	}
	// three started; "gone" lost its session, so it is closed and stopped too, as is "done".
	if kinds[wire.KindStarted] != 3 || kinds[wire.KindStopped] != 2 {
		t.Fatalf("kinds %v", kinds)
	}
	controller.report()
	if queue.Len() != 5 {
		t.Fatalf("a second pass repeated reports: %d", queue.Len())
	}
	sessions.pruned = []recordings.Meta{{ID: sessions.metas[2].ID, EndReason: recordings.ReasonRetention}}
	controller.prune()
	sessions.metas = sessions.metas[:2]
	reopened, err := New(controller.config) // the ledger survives a restart
	if err != nil {
		t.Fatal(err)
	}
	reopened.report()
	last := queue.Oldest(100)[queue.Len()-1]
	if queue.Len() != 6 || last.Kind != wire.KindDeleted || last.Recording.Reason != recordings.ReasonRetention {
		t.Fatalf("deleted report %+v (len %d)", last, queue.Len())
	}
}

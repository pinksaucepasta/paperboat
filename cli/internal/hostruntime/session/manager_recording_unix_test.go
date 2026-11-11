//go:build darwin || linux

package session

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/recordings"
	"github.com/pinksaucepasta/paperboat/internal/recording"
)

func recordingManager(t *testing.T) (*Manager, string, string, *ecdh.PrivateKey) {
	t.Helper()
	manager, root, shell := realManager(t)
	recorder, err := recordings.Open(recordings.Config{Root: filepath.Join(t.TempDir(), "recordings"), SegmentBytes: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Shutdown() })
	manager.config.Recordings = recorder
	team, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return manager, root, shell, team
}

func decryptRecording(t *testing.T, manager *Manager, team *ecdh.PrivateKey, meta recordings.Meta) string {
	t.Helper()
	var text strings.Builder
	for index := uint32(0); index < meta.Segments; index++ {
		file, err := manager.RecordingSegment(meta.ID, index)
		if err != nil {
			t.Fatal(err)
		}
		header, err := recording.ParseSegmentHeader(file)
		if err != nil {
			t.Fatal(err)
		}
		key, err := recording.OpenDataKey(team.Bytes(), meta.ID, header.KeyGeneration, header.SealedKey)
		if err != nil {
			t.Fatal(err)
		}
		_, plaintext, err := recording.OpenSegment(file, key)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(plaintext)
	}
	return text.String()
}

func TestRecordingStartsFromTheScreenAndFollowsOutputAndSize(t *testing.T) {
	manager, root, shell, team := recordingManager(t)
	created, err := manager.Create(context.Background(), CreateRequest{Name: "rec", Command: shellCommand(shell, root, "printf before; read line; printf after; read line")})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background(), created.ID)
	waitLatest(t, manager, created.ID, uint64(len("before")))
	owner := Participant{AttachmentID: "att_1", AccountID: "acct_1", ClientID: "client_1", Role: "owner", ConnectedAt: time.Now()}
	if _, err := manager.AttachParticipant(created.ID, owner, 0); err != nil {
		t.Fatal(err)
	}
	state, err := manager.StartRecording(created.ID, recordings.StartRequest{Trigger: recordings.TriggerManual, ActorAccountID: "acct_1", Key: recordings.Key{Generation: 1, Public: team.PublicKey().Bytes()}, QuotaBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil || snapshot.Recording == nil || snapshot.Recording.ID != state.ID || snapshot.Recording.StartedBy != "acct_1" {
		t.Fatalf("snapshot recording %+v %v", snapshot.Recording, err)
	}
	if err := manager.Resize(created.ID, "att_1", pty.Dimensions{Columns: 120, Rows: 35}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Write(created.ID, InputKey{ClientID: "client_1", AttachmentID: "att_1", Generation: created.Generation, InputID: "inp_0001"}, []byte("go\n")); err != nil {
		t.Fatal(err)
	}
	collectUntil(t, manager, created.ID, "att_1", "after")
	meta, err := manager.StopRecording(created.ID, "acct_1", recordings.ReasonStopped)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, _ := manager.Snapshot(created.ID); snapshot.Recording != nil {
		t.Fatal("REC still set after stop")
	}
	text := decryptRecording(t, manager, team, meta)
	for _, want := range []string{`"height":24`, "before", `"r","120x35"`, "after"} {
		if !strings.Contains(text, want) {
			t.Fatalf("recording lacks %q:\n%s", want, text)
		}
	}
	if !strings.Contains(strings.Join(meta.Accounts, ","), "acct_1") {
		t.Fatalf("accounts %v", meta.Accounts)
	}
}

func TestRecordingEndsWithItsSession(t *testing.T) {
	manager, root, shell, team := recordingManager(t)
	created, err := manager.Create(context.Background(), CreateRequest{Name: "rec", Command: shellCommand(shell, root, "read line")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartRecording(created.ID, recordings.StartRequest{Trigger: recordings.TriggerRule, Key: recordings.Key{Generation: 1, Public: team.PublicKey().Bytes()}, QuotaBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Close(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		metas, err := manager.Recordings()
		if err != nil {
			t.Fatal(err)
		}
		if len(metas) == 1 && metas[0].EndReason == recordings.ReasonSessionEnded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording did not end with its session: %+v", metas)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

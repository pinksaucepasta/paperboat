//go:build darwin || linux

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestActiveWritersKeepInputActivityAndAuthorityAcrossTakeover(t *testing.T) {
	manager, root, shell := realManager(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		manager.Shutdown(cleanup)
	})
	created, err := manager.Create(ctx, CreateRequest{Name: "writers", Command: shellCommand(shell, root, "exec cat")})
	if err != nil {
		t.Fatal(err)
	}
	a := Participant{AttachmentID: "att_a", AccountID: "account", ClientID: "client_a", SourceMachineID: "machine_a", Role: "owner", ConnectedAt: time.Now()}
	b := Participant{AttachmentID: "att_b", AccountID: "account", ClientID: "client_b", SourceMachineID: "machine_b", Role: "owner", ConnectedAt: time.Now()}
	first, err := manager.AttachParticipantAtGeneration(created.ID, a, 0, created.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AttachParticipantAtGeneration(created.ID, b, 0, created.Generation); err != nil {
		t.Fatal(err)
	}
	key := InputKey{ClientID: a.ClientID, AttachmentID: a.AttachmentID, Generation: created.Generation, InputSequence: 1}
	if _, err := manager.Write(created.ID, key, []byte("a\n")); err != nil {
		t.Fatal(err)
	}
	writers, err := manager.ActiveWriters(created.ID)
	if err != nil || len(writers) != 2 {
		t.Fatal("owner writer inventory unavailable", err)
	}
	var inputAt time.Time
	for _, writer := range writers {
		if writer.ClientID == a.ClientID {
			inputAt = writer.LastInput
		}
	}
	if inputAt.IsZero() {
		t.Fatal("accepted input did not record writer activity")
	}
	if _, err := manager.Write(created.ID, key, []byte("a\n")); err != nil {
		t.Fatal(err)
	}
	replacement, err := manager.AttachParticipantAtGeneration(created.ID, a, 0, created.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DetachAttachment(created.ID, a.AttachmentID, first.AttachmentLease); err != nil {
		t.Fatal(err)
	}
	viewer := Participant{AttachmentID: "att_view", AccountID: "account", ClientID: "viewer", SourceMachineID: "machine_viewer", Role: "viewer", ConnectedAt: time.Now()}
	if _, err := manager.AttachParticipantAtGeneration(created.ID, viewer, 0, created.Generation); err != nil {
		t.Fatal(err)
	}
	writers, err = manager.ActiveWriters(created.ID)
	if err != nil || len(writers) != 2 {
		t.Fatal("viewer became a writer or takeover lost a writer", err)
	}
	for _, writer := range writers {
		if writer.ClientID == a.ClientID && (writer.MachineID != a.SourceMachineID || !writer.LastInput.Equal(inputAt)) {
			t.Fatal("input replay or takeover changed writer activity or identity")
		}
	}
	if err := manager.DetachAttachment(created.ID, a.AttachmentID, replacement.AttachmentLease); err != nil {
		t.Fatal(err)
	}
	writers, err = manager.ActiveWriters(created.ID)
	if err != nil || len(writers) != 1 || writers[0].ClientID != b.ClientID {
		t.Fatal("detached writer stayed eligible", err)
	}
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(snapshot)
	if err != nil || bytes.Contains(public, []byte(b.SourceMachineID)) || bytes.Contains(public, []byte(viewer.SourceMachineID)) {
		t.Fatal("private writer metadata leaked into a terminal snapshot", err)
	}
}

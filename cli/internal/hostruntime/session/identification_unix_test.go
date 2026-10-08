//go:build linux || darwin

package session

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerIdentificationSurvivesDetachAndTracksDirectory(t *testing.T) {
	manager, root, shell := realManager(t)
	root, errCanonical := filepath.EvalSymlinks(root)
	if errCanonical != nil {
		t.Fatal(errCanonical)
	}
	sub := filepath.Join(root, "project api")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	script := `printf '\033]2;✳ Fixing auth\007'; read line; cd 'project api'; printf '\033]2;Running tests\033\\'; read line; exec sleep 30`
	created, err := manager.Create(context.Background(), CreateRequest{Name: "backend-work", Command: shellCommand(shell, root, script)})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background(), created.ID)
	awaitIdentification(t, manager, created.ID, func(s Snapshot) bool { return s.Title == "✳ Fixing auth" && s.CurrentDirectory == root })
	attached, err := manager.Attach(created.ID, "att_ident", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(attached.Replay.Events[0].Data, []byte("\x1b]2;")) {
		t.Fatal("observing title removed terminal output")
	}
	key := InputKey{ClientID: "ident-client", AttachmentID: "att_ident", Generation: created.Generation, InputID: "ident-input1"}
	if _, err = manager.Write(created.ID, key, []byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	awaitIdentification(t, manager, created.ID, func(s Snapshot) bool { return s.Title == "Running tests" && s.CurrentDirectory == sub && s.CWD == root })
	if err = manager.Detach(created.ID, "att_ident"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "backend-work" || snapshot.Title != "Running tests" || snapshot.CurrentDirectory != sub || len(snapshot.Participants) != 0 {
		t.Fatalf("detached metadata=%+v", snapshot)
	}
	if _, err = manager.Attach(created.ID, "att_ident2", 0); err != nil {
		t.Fatal(err)
	}
	key.AttachmentID = "att_ident2"
	key.InputID = "ident-input2"
	if _, err = manager.Write(created.ID, key, []byte("run\n")); err != nil {
		t.Fatal(err)
	}
	awaitIdentification(t, manager, created.ID, func(s Snapshot) bool { return s.ForegroundProcess == "sleep" && s.CurrentDirectory == sub })
	if _, err = manager.Close(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = manager.Snapshot(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentDirectory != "" || snapshot.ForegroundProcess != "" {
		t.Fatal("closed process advertised live metadata")
	}
}

func awaitIdentification(t *testing.T, manager *Manager, id string, ready func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := manager.Snapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if ready(snap) {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := manager.Snapshot(id)
	t.Fatalf("identification did not converge: title=%q process=%q dir=%q", snap.Title, snap.ForegroundProcess, snap.CurrentDirectory)
	return Snapshot{}
}

func TestReportedDirectoryDoesNotChangeLaunchCommand(t *testing.T) {
	var tracker identificationTracker
	tracker.Consume([]byte("\x1b]9;9;C:\\Projects\\api\x07"))
	if tracker.directory != `C:\Projects\api` {
		t.Fatalf("Windows directory=%q", tracker.directory)
	}
	tracker.Consume([]byte("\x1b]9;9;\x07"))
	if tracker.directory != "" {
		t.Fatal("non-filesystem prompt retained stale filesystem cwd")
	}
	tracker.Consume([]byte("\x1b]7;file:///C:/Projects/my%20api\x07"))
	if !strings.Contains(tracker.directory, "my api") {
		t.Fatal("Windows URI was not decoded")
	}
}

func TestManagerIdentificationRestartClearsOldApplication(t *testing.T) {
	manager, root, shell := realManager(t)
	root, errCanonical := filepath.EvalSymlinks(root)
	if errCanonical != nil {
		t.Fatal(errCanonical)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Name: "work", Command: shellCommand(shell, root, `printf '\033]2;Old application\007'; read line`)})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background(), created.ID)
	awaitIdentification(t, manager, created.ID, func(s Snapshot) bool { return s.Title == "Old application" })
	if _, err = manager.Close(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := manager.Restart(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Title != "" || restarted.CurrentDirectory != "" || restarted.Generation == created.Generation {
		t.Fatal("old-generation application metadata survived restart")
	}
	awaitIdentification(t, manager, created.ID, func(s Snapshot) bool { return s.Title == "Old application" && s.CurrentDirectory == root })
}

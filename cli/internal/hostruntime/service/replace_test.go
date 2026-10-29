package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestReplaceRestartsUnchangedDeclarationAndRestoresPrevious(t *testing.T) {
	control := &controller{}
	installer, err := New(Config{Platform: "linux", ConfigRoot: t.TempDir(), Executable: executable(t), User: "test", Group: "test", Arguments: []string{"daemon"}, Controller: control})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := installer.Install(ctx); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(installer.DefinitionPath())
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := installer.Replace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !control.applied[len(control.applied)-1] {
		t.Fatal("same-path binary was not restarted")
	}
	if err := rollback(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(installer.DefinitionPath())
	if err != nil || !bytes.Equal(previous, restored) {
		t.Fatal("previous declaration not restored", err)
	}
}

func TestReplaceFailedFirstInstallCanRemoveNewService(t *testing.T) {
	control := &controller{applyErr: errors.New("cannot start")}
	installer, err := New(Config{Platform: "linux", ConfigRoot: t.TempDir(), Executable: executable(t), User: "test", Group: "test", Arguments: []string{"daemon"}, Controller: control})
	if err != nil {
		t.Fatal(err)
	}
	rollback, err := installer.Replace(context.Background())
	if err == nil || rollback == nil {
		t.Fatal("failed activation lost recovery")
	}
	if err := rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(installer.DefinitionPath()); !errors.Is(err, os.ErrNotExist) || control.removed != 1 {
		t.Fatal("failed new service remained installed")
	}
}

func TestReconcileKeepsValidatedDaemonRunningAndRestoresChangedDeclaration(t *testing.T) {
	control := &controller{}
	installer, err := New(Config{Platform: "linux", ConfigRoot: t.TempDir(), Executable: executable(t), User: "test", Group: "test", Arguments: []string{"daemon"}, Controller: control})
	if err != nil {
		t.Fatal(err)
	}
	if err := installer.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(installer.DefinitionPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installer.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if control.applied[len(control.applied)-1] {
		t.Fatal("validated daemon was restarted a second time")
	}
	installer.config.Arguments = []string{"daemon", "--config", "/tmp/example.test/config.json"}
	restore, err := installer.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !control.applied[len(control.applied)-1] {
		t.Fatal("changed daemon declaration was not activated")
	}
	if err := restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(installer.DefinitionPath())
	if err != nil || !bytes.Equal(previous, restored) {
		t.Fatal("changed declaration lost rollback", err)
	}
}

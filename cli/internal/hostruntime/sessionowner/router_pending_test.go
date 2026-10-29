//go:build linux

package sessionowner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

func TestPublishingAnUnstartedOwnerDoesNotInterruptAHeldTerminal(t *testing.T) {
	manager, workspace, shell := newRouterTestManager(t)
	pendingManager, pendingWorkspace, _ := newRouterTestManager(t)
	activeGeneration, pendingGeneration := generation(91), generation(92)
	active := authenticatedRouterOwner(t, activeGeneration, manager, workspace, shell)
	pending := authenticatedRouterOwner(t, pendingGeneration, pendingManager, pendingWorkspace, shell)
	owners := map[string]Owner{activeGeneration: {Descriptor: active.Descriptor}}
	router, err := NewRouter(activeGeneration, owners, 8, ensureRouterOwner(map[string]Owner{activeGeneration: active, pendingGeneration: pending}))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := router.Service()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	created, err := remote.Create(ctx, session.CreateRequest{Name: "held", Command: routerShellCommand(shell, workspace, `printf 'ready'; IFS= read -r input; printf 'held:%s' "$input"; exec sleep 60`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.AttachLive(created.ID, "held-attachment"); err != nil {
		t.Fatal(err)
	}
	owners[pendingGeneration] = Owner{Descriptor: pending.Descriptor}
	if err = router.SetOwners(activeGeneration, owners); err != nil {
		t.Fatal(err)
	}
	// The installer has published the next owner, but its process has not
	// created its store yet. Global inventory must still report this failure.
	router.archive = func(context.Context, string) (workloadbridge.ArchiveMetadata, error) {
		return workloadbridge.ArchiveMetadata{}, os.ErrNotExist
	}
	key := session.InputKey{ClientID: "client", AttachmentID: "held-attachment", Generation: created.Generation, InputID: "held-input"}
	if _, err = remote.Write(created.ID, key, []byte("after-publication\n")); err != nil {
		t.Fatalf("held input depended on unfinished owner: %v", err)
	}
	var output []byte
	var next uint64
	for !bytes.Contains(output, []byte("held:after-publication")) {
		event, e := remote.WaitNext(ctx, created.ID, "held-attachment", next)
		if e != nil {
			t.Fatal(e)
		}
		output = append(output, event.Data...)
		next = event.EndSequence
	}
	if _, err = remote.AttachLive(created.ID, "resumed-attachment"); err != nil {
		t.Fatalf("reattach depended on unfinished owner: %v", err)
	}
	if _, err = router.Call(ctx, "List", json.RawMessage(`{}`)); err == nil {
		t.Fatal("global inventory hid the missing store")
	}
}

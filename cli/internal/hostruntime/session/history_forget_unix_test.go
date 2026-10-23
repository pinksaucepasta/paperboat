//go:build darwin || linux

package session

import (
	"context"
	"fmt"
	"testing"
)

// Every viewer of a shared terminal gets its own attachment; its history acknowledgement was kept
// after it detached, so a long-lived session grew one entry per viewer that ever attached.
func TestDetachedViewersLeaveNoHistoryAcknowledgement(t *testing.T) {
	manager, root, shell := realManager(t)
	created, err := manager.Create(context.Background(), CreateRequest{Name: "shared", Command: shellCommand(shell, root, "exec sleep 30")})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close(context.Background(), created.ID)
	session, err := manager.get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		id := fmt.Sprintf("att_viewer_%02d", i)
		if _, err := manager.Attach(created.ID, id, 0); err != nil {
			t.Fatal(err)
		}
		if err := manager.Acknowledge(created.ID, id, 0); err != nil {
			t.Fatal(err)
		}
		if _, ok := session.history.Acknowledged(id); !ok {
			t.Fatalf("no acknowledgement recorded for %s", id)
		}
		if err := manager.Detach(created.ID, id); err != nil {
			t.Fatal(err)
		}
		if _, ok := session.history.Acknowledged(id); ok {
			t.Fatalf("detached %s kept its history acknowledgement", id)
		}
	}
}

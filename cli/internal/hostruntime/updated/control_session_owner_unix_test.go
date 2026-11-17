//go:build linux || darwin

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/sessionowner"
)

func TestStartSessionOwnerWithoutOwnersIsUnavailable(t *testing.T) {
	var mu sync.Mutex
	if _, err := startSessionOwner(context.Background(), &mu, nil, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestStartSessionOwnerPassesTheCoordinatorsRefusalBack(t *testing.T) {
	var mu sync.Mutex
	failure := errors.New("owners unreadable")
	if _, err := startSessionOwner(context.Background(), &mu, func(context.Context) (sessionowner.Coordinator, error) { return sessionowner.Coordinator{}, failure }, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, failure) {
		t.Fatalf("err = %v", err)
	}
	// A coordinator with no valid active descriptor refuses every generation.
	if _, err := startSessionOwner(context.Background(), &mu, func(context.Context) (sessionowner.Coordinator, error) { return sessionowner.Coordinator{}, nil }, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, sessionowner.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestServeSessionOwnerStartRefusesAnUnprotectedSocketDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	err := ServeSessionOwnerStart(context.Background(), filepath.Join(dir, "control.sock"), 1000, 1000, nil)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
}

// Needs root: the control directory must be root-owned 0755, and the client refuses a server that is not uid 0.
func TestServeSessionOwnerStartAnswersOnlyTheSessionOwnerStartOperation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the control directory must be root-owned")
	}
	dir, err := os.MkdirTemp("", "so")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(dir, "control.sock")
	asked := make(chan struct{}, 1)
	if err := ServeSessionOwnerStart(ctx, socket, 0, 0, func(context.Context) (sessionowner.Coordinator, error) {
		asked <- struct{}{}
		return sessionowner.Coordinator{}, sessionowner.ErrInvalid
	}); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartSessionOwner(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err == nil {
		t.Fatal("a refused start returned no error")
	}
	select {
	case <-asked:
	default:
		t.Fatal("the request never reached the coordinator")
	}
	if _, err := client.Status(ctx); err == nil {
		t.Fatal("an in-process owner-start server answered a status request")
	}
}

//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
)

// F14-02: replacing the daemon ends open SSH connections and access forwards, so an update that
// would do that refuses (and a scheduled one stays pending) unless forced.
func TestUpdateRefusesWhileSSHConnectionsAreOpen(t *testing.T) {
	participants := &participantFixture{open: liveconnections.Counts{SSH: 2, Forwards: 1}}
	service := &Service{config: Config{Participants: participants}}

	err := service.refuseWhileConnectionsAreOpen(context.Background(), "2026.10.21.1", false)
	var busy *autoupdate.ActiveTerminalSessionsError
	if !errors.As(err, &busy) || busy.RequiredVersion != "2026.10.21.1" || busy.SSH != 2 || busy.Forwards != 1 {
		t.Fatalf("an update with open connections was not refused: %v", err)
	}
	if want := "Paperboat is busy: 2 open SSH connections and 1 open port forward. Wait, close them, or use --force."; err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}
	if controlErrorCode(err) != autoupdate.BlockedActiveTerminalSessions {
		t.Fatalf("control code = %q: the scheduler defers on this code", controlErrorCode(err))
	}

	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "2026.10.21.1", true); err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}

	participants.open = liveconnections.Counts{SSH: 1}
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err == nil || !strings.Contains(err.Error(), "1 open SSH connection.") {
		t.Fatalf("singular message = %v", err)
	}

	// Idle: the update installs. A probe that cannot answer never blocks one.
	participants.open = liveconnections.Counts{}
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err != nil {
		t.Fatalf("an idle machine was refused: %v", err)
	}
	participants.open, participants.openErr = liveconnections.Counts{SSH: 3}, errors.New("probe failed")
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err != nil {
		t.Fatalf("a failing probe blocked the update: %v", err)
	}
}

// An SSH connection the worker bridges itself (the operation-based path, no carrier) also makes a
// manual update busy, read over the real hostd socket; a worker that cannot answer drops only its part.
func TestUpdateRefusesWhileTheWorkerBridgesSSH(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "pbup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	token := []byte(strings.Repeat("x", 32))
	config := hostdproto.SocketConfig{SocketPath: filepath.Join(root, "s", "hostd.sock"), StatePath: filepath.Join(root, "f", "fence.json"), UID: os.Geteuid(), GID: os.Getegid(), Token: token, APIMin: 1, APIMax: 1, RequestTimeout: time.Minute, OpenSSH: func(context.Context) (int, error) { return 1, nil }}
	server, err := hostdproto.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	service := &Service{config: Config{Participants: &participantFixture{}, SocketPath: config.SocketPath, Token: token}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = service.refuseWhileConnectionsAreOpen(context.Background(), "v", false)
		var busy *autoupdate.ActiveTerminalSessionsError
		if errors.As(err, &busy) && busy.SSH == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an SSH connection bridged by the worker did not make the update busy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", true); err != nil {
		t.Fatalf("--force must proceed: %v", err)
	}
	service.config.SocketPath = filepath.Join(root, "missing.sock")
	if err := service.refuseWhileConnectionsAreOpen(context.Background(), "v", false); err != nil {
		t.Fatalf("an unreachable worker blocked the update: %v", err)
	}
}

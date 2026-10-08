//go:build windows

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"golang.org/x/sys/windows"
)

// A real private pipe and real PE bytes verify the temporary control owner;
// no SCM service or enrolled installation is touched.
func TestWindowsActivatorControlStatusSettingsAndRetirement(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	journal := testWindowsActivationJournal()
	journal.Runtime.Path = executable
	journal.Runtime.SHA256 = hex.EncodeToString(digest[:])
	journal.Runtime.Length = int64(len(body))
	journal.Candidate.SHA256 = journal.Runtime.SHA256
	journal.Candidate.Length = journal.Runtime.Length
	var journalMu sync.Mutex
	oldLoad := loadWindowsActivationJournalForController
	loadWindowsActivationJournalForController = func(WindowsConfig) (windowsActivationJournal, error) {
		journalMu.Lock()
		defer journalMu.Unlock()
		return journal, nil
	}
	t.Cleanup(func() { loadWindowsActivationJournalForController = oldLoad })
	config := WindowsConfig{OwnerSID: user.User.Sid.String(), StateRoot: t.TempDir(), AutomaticChecks: true, ControlSocket: fmt.Sprintf(`\\.\pipe\paperboat-activator-test-%d-%d`, os.Getpid(), time.Now().UnixNano())}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	control, err := startWindowsActivatorControl(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })
	client, err := NewClient(config.ControlSocket, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []windowsActivationStage{windowsActivationStaged, windowsActivationSwitching, windowsActivationServicesLive, windowsActivationRollingBack} {
		journalMu.Lock()
		journal.Stage = stage
		journalMu.Unlock()
		response, err := client.Status(context.Background())
		if err != nil || !response.Pending || response.Candidate == nil || response.Candidate.ID != journal.Candidate.ID || response.Transaction.CandidateVersion != journal.Version {
			t.Fatalf("stage=%s response=%+v err=%v", stage, response, err)
		}
		if response.Observation.CheckedAt.IsZero() == false {
			t.Fatal("activator invented a scheduler observation")
		}
	}
	settings := autoupdate.DefaultPreferences(false)
	settings.LocalTime = "17:23"
	if response, err := client.Settings(context.Background(), &settings); err != nil || response.Settings == nil || *response.Settings != settings {
		t.Fatalf("settings write: %+v %v", response, err)
	}
	if response, err := client.Settings(context.Background(), nil); err != nil || response.Settings == nil || *response.Settings != settings {
		t.Fatalf("settings read: %+v %v", response, err)
	}
	if journal.ApprovedCandidateID != journal.Candidate.ID {
		t.Fatal("opt-out changed approved recovery ownership")
	}
	if response, err := client.Check(context.Background()); err == nil || response.ErrorCode != "activation_unavailable" {
		t.Fatalf("activator accepted another transaction: %+v %v", response, err)
	}
	// Closing must interrupt an incomplete request as well as the accept loop.
	connection, err := winio.DialPipeContext(context.Background(), config.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("{")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- control.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control close did not join active connection")
	}
	replacement, err := winio.ListenPipe(config.ControlSocket, &winio.PipeConfig{})
	if err != nil {
		t.Fatalf("native replacement cannot acquire pipe: %v", err)
	}
	replacement.Close()
}

func TestWindowsActivatorControlCancellationReleasesPipe(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	config := WindowsConfig{OwnerSID: user.User.Sid.String(), ControlSocket: fmt.Sprintf(`\\.\pipe\paperboat-activator-cancel-%d-%d`, os.Getpid(), time.Now().UnixNano())}
	ctx, cancel := context.WithCancel(context.Background())
	control, err := startWindowsActivatorControl(ctx, config)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- control.Close() }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled control did not stop")
	}
	replacement, err := winio.ListenPipe(config.ControlSocket, &winio.PipeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
}

func TestWindowsActivatorControlDoesNotStealRestoredNativeEndpoint(t *testing.T) {
	journal := testWindowsActivationJournal()
	for _, maintenance := range []bool{false, true} {
		journal.Release.SupervisorMaintenance = maintenance
		for _, stage := range []windowsActivationStage{windowsActivationCommitted, windowsActivationRolledBack} {
			journal.Stage = stage
			if windowsActivatorNeedsControl(journal) {
				t.Fatalf("terminal %s steals restored control", stage)
			}
		}
		for _, stage := range []windowsActivationStage{windowsActivationServicesLive, windowsActivationCommitReady, windowsActivationRollbackReady} {
			journal.Stage = stage
			if got := windowsActivatorNeedsControl(journal); got == maintenance {
				t.Fatalf("stage=%s maintenance=%t temporary=%t", stage, maintenance, got)
			}
		}
	}
}

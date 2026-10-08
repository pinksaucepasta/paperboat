//go:build windows

package process

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsPromptIdentification(t *testing.T) {
	for _, name := range []string{"powershell.exe", "cmd.exe"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			sub := filepath.Join(root, "project api")
			if err := os.Mkdir(sub, 0700); err != nil {
				t.Fatal(err)
			}
			system := os.Getenv("SystemRoot")
			shell := filepath.Join(system, "System32", name)
			if name == "powershell.exe" {
				shell = filepath.Join(system, "System32", "WindowsPowerShell", "v1.0", name)
			}
			adapter, err := pty.NewAdapter(root)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := session.NewManager(session.ManagerConfig{Launch: func(c pty.Command) (session.PTYProcess, error) { return adapter.Start(c) }})
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Shutdown(context.Background())
			env, err := BaseEnvironment(shell)
			if err != nil {
				t.Fatal(err)
			}
			launcher, err := NewShellLauncher(shell, env, manager)
			if err != nil {
				t.Fatal(err)
			}
			created, err := launchWithServiceHandles(func() (session.Snapshot, error) {
				return launcher.Launch(context.Background(), LaunchRequest{ID: "ident_windows", Name: "editable-name", CWD: root, Dimensions: pty.Dimensions{Columns: 100, Rows: 30}})
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitWindowsMetadata(t, manager, created.ID, func(s session.Snapshot) bool { return s.CurrentDirectory != "" })
			if _, err = manager.Attach(created.ID, "ident_attachment", 0); err != nil {
				t.Fatal(err)
			}
			command := "cd /d \"" + sub + "\"\r"
			if name == "powershell.exe" {
				command = "Set-Location -LiteralPath '" + strings.ReplaceAll(sub, "'", "''") + "'\r"
			}
			key := session.InputKey{ClientID: "ident-client", AttachmentID: "ident_attachment", Generation: created.Generation, InputID: "input1"}
			if _, err = manager.Write(created.ID, key, []byte(command)); err != nil {
				t.Fatal(err)
			}
			awaitWindowsMetadata(t, manager, created.ID, func(s session.Snapshot) bool {
				return strings.EqualFold(strings.TrimPrefix(strings.ReplaceAll(s.CurrentDirectory, "\\", "/"), "/"), strings.ReplaceAll(sub, "\\", "/"))
			})
			if name == "powershell.exe" {
				key.InputID = "input2"
				command = "[Console]::Write(([char]27).ToString()+']2;Running tests'+[char]7); Start-Sleep -Seconds 2\r"
				if _, err = manager.Write(created.ID, key, []byte(command)); err != nil {
					t.Fatal(err)
				}
				awaitWindowsMetadata(t, manager, created.ID, func(s session.Snapshot) bool { return s.Title == "Running tests" })
				awaitWindowsMetadata(t, manager, created.ID, func(s session.Snapshot) bool { return s.Title == "" })
			}
			snapshot, err := manager.Snapshot(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Name != "editable-name" || snapshot.CWD != root {
				t.Fatal("metadata changed identity or launch directory")
			}
		})
	}
}

func awaitWindowsMetadata(t *testing.T, manager *session.Manager, id string, ready func(session.Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := manager.Snapshot(id)
		if err != nil {
			t.Fatal(err)
		}
		if ready(snap) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shell did not publish expected terminal metadata")
}

// Exercise a console-less parent with null handles, restoring the SSH test
// runner immediately. Actual enrolled services supply private NUL handles;
// that distinct boundary is covered by the native PTY service-parent test.
func launchWithServiceHandles(launch func() (session.Snapshot, error)) (session.Snapshot, error) {
	ids := []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE}
	saved := make([]windows.Handle, len(ids))
	for i, id := range ids {
		handle, err := windows.GetStdHandle(id)
		if err != nil {
			return session.Snapshot{}, err
		}
		saved[i] = handle
	}
	defer func() {
		for i, id := range ids {
			_ = windows.SetStdHandle(id, saved[i])
		}
	}()
	for _, id := range ids {
		if err := windows.SetStdHandle(id, 0); err != nil {
			return session.Snapshot{}, err
		}
	}
	return launch()
}

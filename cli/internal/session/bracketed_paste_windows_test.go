//go:build windows

package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/paste"
)

// conPTY starts a Windows program as a target's terminal does; its output goes through remote, as pb connect's does.
func conPTY(t *testing.T, remote *RemotePaste, program string, args ...string) (*pty.Process, func(string)) {
	t.Helper()
	root := t.TempDir()
	adapter, err := pty.NewAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	process, err := adapter.Start(pty.Command{Path: filepath.Join(os.Getenv("WINDIR"), "System32", program), Args: args, CWD: root,
		Dimensions: pty.Dimensions{Columns: 120, Rows: 25}})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var screen bytes.Buffer
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := process.Read(buffer)
			shown := remote.follow(buffer[:n])
			mu.Lock()
			screen.Write(shown)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { // the reader above drains the output that closing a pseudoconsole waits for
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := process.Terminate(ctx, 0); err != nil {
			t.Errorf("terminate: %v", err)
		}
		_ = process.CloseIO()
	})
	return process, func(text string) {
		t.Helper()
		body := func() string {
			mu.Lock()
			defer mu.Unlock()
			return screen.String()
		}
		waitUntil(t, func() string { return fmt.Sprintf("%q in ConPTY output %q", text, body()) },
			func() bool { return strings.Contains(body(), text) })
	}
}

// cmd.exe never asks for bracketed paste: it gets a paste, and a dropped file's remote path, as plain text.
func TestPasteAndDropIntoCmdUnderConPTY(t *testing.T) {
	dropped := filepath.Join(t.TempDir(), "drop.txt")
	if err := os.WriteFile(dropped, []byte("drop"), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := &RemotePaste{}
	process, shows := conPTY(t, remote, "cmd.exe")
	policy, lifecycle, delivered := dropPolicy(`C:\pb-remote\drop.txt`)
	input := paste.NewWithPolicy(process, policy, lifecycle, paste.WithRemoteBracketedPaste(remote.Enabled))
	t.Cleanup(func() { input.Abort(); _ = input.Close() })
	send := func(text string) {
		if _, err := input.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	shows(">")
	// The caret splits each marker, so only the line cmd prints matches it.
	send("\x1b[200~echo PB_^PASTE\x1b[201~\r")
	shows("PB_PASTE")
	send("echo drop^=")
	send("\x1b[200~" + dropped + "\x1b[201~")
	select {
	case <-delivered:
	case <-time.After(20 * time.Second):
		t.Fatal("the dropped file was not uploaded")
	}
	send("\r")
	shows(`drop=C:\pb-remote\drop.txt`)
	if remote.Enabled() {
		t.Fatal("cmd.exe was recorded as asking for bracketed paste")
	}
}

// A Windows program that does ask is heard through ConPTY, so its pastes are framed.
func TestConPTYPassesBracketedPasteModeThrough(t *testing.T) {
	remote := &RemotePaste{}
	_, shows := conPTY(t, remote, `WindowsPowerShell\v1.0\powershell.exe`, "-NoLogo", "-NoProfile", "-Command",
		"[Console]::Write([string][char]27 + '[?2004h' + 'PB_' + 'ON'); Start-Sleep 5; [Console]::Write([string][char]27 + '[?2004l' + 'PB_' + 'OFF'); Start-Sleep 30")
	shows("PB_ON")
	waitUntil(t, func() string { return "the program's request for bracketed paste" }, remote.Enabled)
	shows("PB_OFF")
	waitUntil(t, func() string { return "the program turning bracketed paste off" }, func() bool { return !remote.Enabled() })
}

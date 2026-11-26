//go:build !windows

package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/pinksaucepasta/paperboat/cli/internal/paste"
)

// shellConn is a real shell on a pseudo-terminal in place of the remote machine.
type shellConn struct {
	*os.File
	cmd *exec.Cmd
}

func (c shellConn) Resize(rows, cols uint16) error {
	return pty.Setsize(c.File, &pty.Winsize{Rows: rows, Cols: cols})
}
func (c shellConn) Close() error       { _ = c.cmd.Process.Kill(); return c.File.Close() }
func (c shellConn) Wait() (int, error) { _ = c.cmd.Wait(); return 0, nil }

type screenBuffer struct {
	mu   sync.Mutex
	body bytes.Buffer
}

func (s *screenBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}

func (s *screenBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}

// The local terminal brackets every paste while pb is attached. A real shell gets the markers only if it asked for them
// (bash 5, zsh) and the plain text otherwise (bash 3.2, as on macOS); a dropped file arrives as its remote path in both.
func TestPasteAndDropIntoShells(t *testing.T) {
	major, err := exec.Command("/bin/bash", "-c", "printf %s ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Skipf("no /bin/bash: %v", err)
	}
	dir := t.TempDir()
	dropped := filepath.Join(dir, "drop.txt")
	inputrc := func(name, setting string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("set enable-bracketed-paste "+setting+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return "INPUTRC=" + path
	}
	if err := os.WriteFile(dropped, []byte("drop"), 0o600); err != nil {
		t.Fatal(err)
	}
	bash := []string{"/bin/bash", "--norc", "--noprofile", "-i"}
	for _, tc := range []struct {
		name string
		argv []string
		env  string
		asks bool
	}{
		{"bash without bracketed paste", bash, inputrc("off", "off"), false},
		{"bash", bash, inputrc("on", "on"), string(major) >= "5"},
		{"zsh", []string{"/bin/zsh", "-f", "-i"}, "ZDOTDIR=" + dir, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := os.Stat(tc.argv[0]); err != nil {
				t.Skip(err)
			}
			terminal, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			oldIn := os.Stdin
			os.Stdin = tty
			defer func() { os.Stdin = oldIn; _, _ = terminal.Close(), tty.Close() }()
			shell := exec.Command(tc.argv[0], tc.argv[1:]...)
			shell.Env = append(os.Environ(), "TERM=xterm-256color", tc.env)
			master, err := pty.Start(shell)
			if err != nil {
				t.Fatal(err)
			}
			conn := shellConn{master, shell}
			remote, screen := &RemotePaste{}, &screenBuffer{}
			policy, lifecycle, delivered := dropPolicy("/tmp/pb-remote-drop")
			input := paste.NewWithPolicy(conn, policy, lifecycle, paste.WithDirectInput(), paste.WithRemoteBracketedPaste(remote.Enabled))
			done := make(chan error, 1)
			go func() {
				_, err := Run(context.Background(), conn, input, WithOutput(screen), WithBracketedPaste(remote))
				done <- err
			}()
			defer func() { _ = conn.Close(); <-done }()
			send := func(text string) {
				if _, err := terminal.WriteString(text); err != nil {
					t.Fatal(err)
				}
			}
			shows := func(text string) {
				t.Helper()
				waitUntil(t, func() string { return fmt.Sprintf("%q in %q", text, screen.String()) },
					func() bool { return strings.Contains(screen.String(), text) })
			}
			atPrompt := func() {
				if tc.asks {
					waitUntil(t, func() string { return "the shell to ask for bracketed paste" }, remote.Enabled)
				} else {
					time.Sleep(time.Second)
				}
			}
			shows(bracketedPasteEnableSequence) // the local terminal is in raw mode with bracketed paste on
			send("echo PB_$((1+1))\r")
			shows("PB_2")
			atPrompt()
			if remote.Enabled() != tc.asks {
				t.Fatalf("remote asked = %v, want %v", remote.Enabled(), tc.asks)
			}
			send("\x1b[200~echo PB_$((2+2))\x1b[201~\r")
			shows("PB_4")
			atPrompt()
			send("printf '<%s>\\n' ")
			send("\x1b[200~" + dropped + "\x1b[201~")
			select {
			case <-delivered:
			case <-time.After(20 * time.Second):
				t.Fatal("the dropped file was not uploaded")
			}
			send("\r")
			shows("</tmp/pb-remote-drop>")
			if tc.asks { // two pasted lines wait for Enter
				atPrompt()
				send("\x1b[200~echo PB_$((3+3))\recho PB_$((4+4))\x1b[201~")
				time.Sleep(time.Second)
				if strings.Contains(screen.String(), "PB_6") {
					t.Fatalf("pasted lines ran before Enter: %q", screen.String())
				}
				send("\r")
				shows("PB_8")
			}
			if out := screen.String(); strings.Contains(out, "00~") || strings.Contains(out, "01~") {
				t.Fatalf("paste markers reached the shell as text: %q", out)
			}
		})
	}
}

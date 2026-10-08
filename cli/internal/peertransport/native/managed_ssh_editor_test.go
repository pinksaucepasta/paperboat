package native_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// Remote-SSH 0.128.0 uses non-PTY shell bootstrap over stdin, SCP for a
// locally downloaded server, dynamic TCP forwarding (or -L for socket mode),
// and fresh SSH processes after disconnect. File editing runs in its backend
// over those forwarded streams, not through a separate SSH file protocol.
func checkRemoteSSHOperations(t *testing.T, ctx context.Context, common []string, target, echoAddress string, disconnect func()) {
	args := func(extra ...string) []string { return append(append([]string{}, common...), extra...) }
	root := filepath.Join(t.TempDir(), "backend with spaces")
	pidFile := filepath.Join(root, "pid")
	t.Cleanup(func() {
		data, _ := os.ReadFile(pidFile)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
	})
	t.Run("EditorBootstrapAndByteStreams", func(t *testing.T) {
		// Use a real detached process to verify that bootstrap EOF does not kill
		// the backend. All generated files and the exact PID belong to this test.
		script := "set -eu\ntest ! -t 0\ntest ! -t 1\nmkdir -p \"$1\"\nnohup sleep 60 >/dev/null 2>&1 </dev/null &\nprintf '%s' $! >\"$1/pid\"\nprintf 'bootstrap-ready\\n'\n"
		output := runOpenSSH(t, ctx, "ssh", []byte(script), args("-T", target, "sh -s -- '"+root+"'")...)
		if string(output) != "bootstrap-ready\n" {
			t.Fatalf("bootstrap output=%q", output)
		}
		payload := make([]byte, 64*1024)
		for i := range payload {
			payload[i] = byte(i)
		}
		if output := runOpenSSH(t, ctx, "ssh", payload, args("-T", target, "cat")...); !bytes.Equal(output, payload) {
			t.Fatal("non-PTY stdin/EOF/stdout changed binary bytes")
		}
		command := exec.CommandContext(ctx, "ssh", args("-T", target, "printf diagnostic >&2; exit 37")...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 37 || stdout.Len() != 0 || stderr.String() != "diagnostic" {
			t.Fatalf("remote exit/stderr mismatch: exit=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
	})
	t.Run("EditorDynamicForwardAndReconnect", func(t *testing.T) {
		checkEditorForward(t, ctx, common, target, "-D", echoAddress, disconnect)
		checkEditorForward(t, ctx, common, target, "-D", echoAddress, nil)
		output := runOpenSSH(t, ctx, "ssh", nil, args("-T", target, "kill -0 \"$(cat '"+pidFile+"')\" && printf backend-survived")...)
		if string(output) != "backend-survived" {
			t.Fatal("detached backend did not survive bootstrap EOF and native disconnect")
		}
	})
	t.Run("EditorSocketForward", func(t *testing.T) {
		listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "backend.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				go func() { defer connection.Close(); _, _ = io.Copy(connection, connection) }()
			}
		}()
		checkEditorForward(t, ctx, common, target, "-L", listener.Addr().String(), nil)
	})
	t.Run("EditorOptionalPTY", func(t *testing.T) {
		output := runOpenSSH(t, ctx, "ssh", nil, args("-tt", target, "test -t 0 && printf pty-ready")...)
		if !bytes.HasPrefix(output, []byte("pty-ready")) {
			t.Fatal("explicit PTY allocation failed")
		}
	})
}

func checkEditorForward(t *testing.T, ctx context.Context, common []string, target, flag, destination string, disconnect func()) {
	t.Helper()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	forward := address
	if flag == "-L" {
		forward += ":" + destination
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := append(append([]string{}, common...), "-T", "-o", "ExitOnForwardFailure=yes", flag, forward, target, "sh")
	command := exec.CommandContext(childCtx, "ssh", args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waited := false
	defer func() {
		cancel()
		if !waited {
			<-done
		}
	}()
	// Match Remote-SSH's -T -D ... host sh shape: the bootstrap channel stays
	// open alongside forwarded channels until transport interruption/cleanup.
	if _, err := io.WriteString(stdin, "printf 'forward-shell-ready\\n'\n"); err != nil {
		t.Fatal(err)
	}
	bootstrapOutput, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || bootstrapOutput != "forward-shell-ready\n" {
		t.Fatalf("forwarding and piped shell bootstrap did not coexist: %q (%v)", bootstrapOutput, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("editor SSH forward did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// VS Code multiplexes management, extension-host and user-forward traffic.
	// Check concurrent channels with distinct binary payloads on one SSH process.
	results := make(chan error, 4)
	for channel := 0; channel < 4; channel++ {
		go func() {
			dialer := &net.Dialer{Timeout: time.Second}
			var connection net.Conn
			var err error
			if flag == "-D" {
				var socks proxy.Dialer
				socks, err = proxy.SOCKS5("tcp", address, nil, dialer)
				if err == nil {
					connection, err = socks.(proxy.ContextDialer).DialContext(ctx, "tcp", destination)
				}
			} else {
				connection, err = dialer.DialContext(ctx, "tcp", address)
			}
			if err != nil {
				results <- err
				return
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			payload := bytes.Repeat([]byte{0, byte(channel), 255, 13, 10}, 800)
			if _, err = connection.Write(payload); err == nil {
				response := make([]byte, len(payload))
				_, err = io.ReadFull(connection, response)
				if err == nil && !bytes.Equal(payload, response) {
					err = fmt.Errorf("forward channel %d changed binary bytes", channel)
				}
			}
			results <- err
		}()
	}
	for channel := 0; channel < 4; channel++ {
		if err := <-results; err != nil {
			t.Errorf("editor SSH forward: %v", err)
		}
	}
	if disconnect != nil {
		disconnect()
		select {
		case err := <-done:
			waited = true
			if err == nil {
				t.Error("interrupted SSH forwarding process unexpectedly succeeded")
			}
		// Match managedssh's existing five-second command shutdown bound;
		// race instrumentation makes the shorter listener-startup bound too
		// small for the SSH process plus its proxy child to drain.
		case <-time.After(5 * time.Second):
			t.Error("interrupted SSH forwarding process did not terminate")
		}
	}
}

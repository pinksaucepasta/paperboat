package native_test

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// OpenSSH leaves ProxyCommand stdin open until it sees output EOF. A proxy
// must therefore exit on native transport EOF without waiting for stdin EOF.
func checkNativeProxyEOF(t *testing.T, ctx context.Context, path string, disconnect func()) {
	t.Helper()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(child, path)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		banner, err := bufio.NewReader(output).ReadString('\n')
		if err != nil || !strings.HasPrefix(banner, "SSH-2.0-") {
			cancel()
			_ = command.Wait()
			done <- fmt.Errorf("native SSH banner unavailable: %w", err)
			return
		}
		disconnect()
		done <- command.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("proxy failed before publishing transport EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("proxy retained stdout after native EOF while SSH input stayed open")
	}
}

// This fixture carries SSH bytes over the native TCP adapter. Its output owner
// exits on transport EOF even while OpenSSH keeps the input pipe open. netcat
// waits for both sides and can hide clean FIN, while a reset happens to exit.
func nativeSSHProxyScript(host, port string) string {
	return fmt.Sprintf(`#!/usr/bin/env python3
import os, socket, sys, threading
connection = socket.create_connection((%q, int(%q)), timeout=5)
connection.settimeout(60)
def send_input():
    try:
        while True:
            data = os.read(0, 65536)
            if not data:
                connection.shutdown(socket.SHUT_WR)
                return
            connection.sendall(data)
    except OSError:
        return
threading.Thread(target=send_input, daemon=True).start()
try:
    while True:
        data = connection.recv(65536)
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
finally:
    connection.close()
`, host, port)
}

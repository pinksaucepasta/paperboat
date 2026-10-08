package selfhost

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorRestartsAndRetiresOwnedChildren(t *testing.T) {
	dir, _, _, server := testInstallation(t, false)
	server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Listen = address
	claim := testClaim(false)
	s.Claim = &claim
	if err := applyClaim(context.Background(), dir, &s); err != nil {
		t.Fatal(err)
	}
	binaries := t.TempDir()
	script := `#!/bin/sh
state=$3
if [ ! -f "$state/restarted" ]; then touch "$state/restarted"; exit 1; fi
echo $$ > "$state/parent.pid"
sleep 60 &
child=$!
echo "$child" > "$state/child.pid"
trap 'kill "$child" 2>/dev/null || true; wait; exit 0' TERM INT
wait "$child"
`
	if err := os.WriteFile(filepath.Join(binaries, "paperboat-relay"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() { done <- Run(ctx, dir, binaries, &logs) }()
	deadline := time.Now().Add(4 * time.Second)
	var child, parent int
	for {
		b, e1 := os.ReadFile(filepath.Join(dir, "relay", "child.pid"))
		p, e2 := os.ReadFile(filepath.Join(dir, "relay", "parent.pid"))
		if e1 == nil && e2 == nil {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			parent, _ = strconv.Atoi(strings.TrimSpace(string(p)))
			if child > 0 && parent > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("runtime restart did not create child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("supervisor cancellation did not finish")
	}
	for _, pid := range []int{parent, child} {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("owned process %d survived cancellation", pid)
		}
	}
	if !strings.Contains(logs.String(), "restarting in 1s") {
		t.Fatal("runtime crash recovery missing")
	}
}

func TestSupervisorCertificateRenewalRestartsOwnedRuntime(t *testing.T) {
	dir, _, _, server := testInstallation(t, false)
	server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Config.Listen = address
	claim := testClaim(false)
	s.Claim = &claim
	if err := applyClaim(context.Background(), dir, &s); err != nil {
		t.Fatal(err)
	}
	binaries := t.TempDir()
	script := `#!/bin/sh
echo $$ > "$3/parent.pid"
sleep 60 &
child=$!
trap 'kill "$child" 2>/dev/null || true; wait; exit 0' TERM INT
wait "$child"
`
	if err := os.WriteFile(filepath.Join(binaries, "paperboat-relay"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	renewals := make(chan time.Time, 1)
	var logs bytes.Buffer
	go func() { done <- run(ctx, dir, binaries, &logs, renewals) }()
	readPID := func(previous int) int {
		deadline := time.Now().Add(3 * time.Second)
		for {
			b, e := os.ReadFile(filepath.Join(dir, "relay", "parent.pid"))
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if e == nil && pid > 0 && pid != previous {
				return pid
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatal("runtime did not start/restart")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first := readPID(0)
	key, err := privateKey(s)
	if err != nil {
		t.Fatal(err)
	}
	old, err := certificatePEM(key, s.Config.EndpointHost, time.Now().AddDate(-1, 0, 15))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "tls.crt"), old); err != nil {
		t.Fatal(err)
	}
	renewals <- time.Now()
	second := readPID(first)
	if err := syscall.Kill(first, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("old runtime survived certificate replacement")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(second, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("renewed runtime survived cancellation")
	}
	if !strings.Contains(logs.String(), "same TLS identity") {
		t.Fatal("renewal restart was not recorded")
	}
}

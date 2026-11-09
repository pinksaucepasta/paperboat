package selfhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A retry of the applied claim that carries a rotated issuer JWKS rewrites the runtime's key file and
// asks the supervisor to rerun the components; a plain retry changes nothing and asks nothing.
func TestClaimRetryAppliesRotatedTrustDocuments(t *testing.T) {
	dir, secret, client, server := testInstallation(t, false)
	var applied atomic.Int32
	server.Config.Handler = Handler(dir, func() { applied.Add(1) }, nil)
	claim := testClaim(false)
	if status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, claim); status != 200 || applied.Load() != 1 {
		t.Fatalf("claim status %d, applied %d", status, applied.Load())
	}
	if status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, claim); status != 200 || applied.Load() != 1 {
		t.Fatalf("plain retry status %d, applied %d", status, applied.Load())
	}
	claim.JWKS = json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"key-rotated","x":"cm90YXRlZA"}]}`)
	if status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, claim); status != 200 || applied.Load() != 2 {
		t.Fatalf("rotated retry status %d, applied %d", status, applied.Load())
	}
	got, err := os.ReadFile(filepath.Join(dir, "relay", "jwks.json"))
	if err != nil || !bytes.Equal(got, claim.JWKS) {
		t.Fatalf("runtime JWKS %q (err %v), want the rotated one", got, err)
	}
}

// The supervisor reruns running components when a claim retry rebuilt their runtime files.
func TestSupervisorRerunsComponentsOnRotatedTrustDocuments(t *testing.T) {
	dir, secret, client, server := testInstallation(t, false)
	claim := testClaim(false)
	if status, _ := post(t, client, server.URL+"/v1/selfhost/claim", secret, claim); status != 200 {
		t.Fatalf("claim status %d", status)
	}
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
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	binaries := t.TempDir()
	script := "#!/bin/sh\necho $$ > \"$3/parent.pid\"\nsleep 60 &\nchild=$!\ntrap 'kill \"$child\" 2>/dev/null || true; wait; exit 0' TERM INT\nwait \"$child\"\n"
	if err := os.WriteFile(filepath.Join(binaries, "paperboat-relay"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() { done <- run(ctx, dir, binaries, &logs, make(chan time.Time), nil) }()
	readPID := func(previous int) int {
		deadline := time.Now().Add(5 * time.Second)
		for {
			b, e := os.ReadFile(filepath.Join(dir, "relay", "parent.pid"))
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			if e == nil && pid > 0 && pid != previous {
				return pid
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatalf("runtime did not start/rerun: previous=%d", previous)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first := readPID(0)
	claim.JWKS = json.RawMessage(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"key-rotated","x":"cm90YXRlZA"}]}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, _ := post(t, client, "https://"+address+"/v1/selfhost/claim", secret, claim)
		if status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated claim status %d", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	readPID(first)
	if err := syscall.Kill(first, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("component kept running on the old trust documents")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

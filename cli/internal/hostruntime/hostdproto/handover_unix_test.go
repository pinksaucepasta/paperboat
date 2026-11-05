//go:build darwin || linux

package hostdproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const handoverHelperEnv = "PAPERBOAT_TEST_HANDOVER_OLD_OWNER"

// TestHandoverMovesListenerWithoutRefusingConnections runs the old owner as a
// separate process: a request it accepted before the handover finishes there,
// requests after commit reach the new owner, no connection is refused in
// between, and the old process exits once its last request is done.
func TestHandoverMovesListenerWithoutRefusingConnections(t *testing.T) {
	dir := shortSocketDir(t)
	token := bytes.Repeat([]byte{0x42}, installationTokenBytes)
	old := exec.Command(os.Args[0], "-test.run=^TestHandoverHelperOldOwner$")
	old.Env = append(os.Environ(), handoverHelperEnv+"="+dir, "PAPERBOAT_TEST_HANDOVER_TOKEN="+hex.EncodeToString(token))
	old.Stderr = os.Stderr
	stdout, err := old.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "ready ") {
		t.Fatalf("old owner did not become ready: %q", lines.Text())
	}
	address := strings.TrimPrefix(lines.Text(), "ready ")
	go func() {
		for lines.Scan() {
		}
		exited <- old.Wait()
	}()
	t.Cleanup(func() { _ = old.Process.Kill() })

	hammer := startHammer(address)
	// A long request on the old owner, started before the handover.
	body, bodyWriter := io.Pipe()
	slow, err := http.Post("http://"+address+"/slow", "text/plain", body)
	if err != nil {
		t.Fatal(err)
	}
	if slow.Header.Get("X-Owner") != "old" {
		t.Fatalf("slow request owner = %q, want old", slow.Header.Get("X-Owner"))
	}

	handover, files, err := DialHandover(context.Background(), HandoverPath(filepath.Join(dir, "h.sock")), os.Geteuid(), token, 1)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.FileListener(files[0])
	if err != nil || listener.Addr().String() != address {
		t.Fatalf("inherited listener = %v, %v; want %s", listener, err, address)
	}
	_ = files[0].Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "new") })}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	fixed, err := handover.Commit(1)
	if err != nil {
		t.Fatal(err)
	}
	_ = fixed[0].Close()
	_ = handover.Close()

	// After commit the old owner stops accepting: every new request is ours.
	deadline := time.Now().Add(5 * time.Second)
	for hammer.lastOwners(20) != "new" {
		if time.Now().After(deadline) {
			t.Fatalf("requests did not move to the new owner: %v", hammer.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-exited:
		t.Fatalf("old owner exited with a request in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = bodyWriter.Close()
	reply, err := io.ReadAll(slow.Body)
	if err != nil || string(reply) != "done" {
		t.Fatalf("slow request = %q, %v; want done from the old owner", reply, err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("old owner exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("old owner did not exit after its last request")
	}
	owners, failures := hammer.stop()
	if len(failures) != 0 {
		t.Fatalf("requests failed during the handover: %v", failures)
	}
	if owners["old"] == 0 || owners["new"] == 0 {
		t.Fatalf("owners = %v, want both", owners)
	}
}

// TestHandoverHelperOldOwner is the old generation's process for the test above.
func TestHandoverHelperOldOwner(t *testing.T) {
	dir := os.Getenv(handoverHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	token, _ := hex.DecodeString(os.Getenv("PAPERBOAT_TEST_HANDOVER_TOKEN"))
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runtimeFile, _ := tcp.(*net.TCPListener).File()
	fixed, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "h.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	fixedFile, _ := fixed.File()
	handover, err := net.ListenUnix("unix", &net.UnixAddr{Name: HandoverPath(filepath.Join(dir, "h.sock")), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int64
	server := &http.Server{ConnState: func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			connections.Add(1)
		case http.StateClosed, http.StateHijacked:
			connections.Add(-1)
		}
	}, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Owner", "old")
		if r.URL.Path != "/slow" {
			_, _ = io.WriteString(w, "old")
			return
		}
		// Send the owner header before the body ends (HTTP/1 reads the body first otherwise).
		controller := http.NewResponseController(w)
		_ = controller.EnableFullDuplex()
		_ = controller.Flush()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "done")
	})}
	served := make(chan struct{})
	go func() { _ = server.Serve(tcp); close(served) }()
	fmt.Printf("ready %s\n", tcp.Addr())
	if err := ServeHandover(context.Background(), handover, HandoverConfig{UID: os.Geteuid(), Token: token, Start: []*os.File{runtimeFile}, Commit: []*os.File{fixedFile}}); err != nil {
		t.Fatal(err)
	}
	// Drain as runtime HTTPService.Shutdown does: http.Server.Shutdown would drop a connection this
	// process accepted whose request arrives after it starts.
	_ = tcp.Close()
	<-served
	deadline := time.Now().Add(HandoverDrainLimit)
	for server.SetKeepAlivesEnabled(false); connections.Load() > 0; server.SetKeepAlivesEnabled(false) {
		if time.Now().After(deadline) {
			t.Fatal("old owner did not drain its connections")
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(0)
}

// TestHandoverRejectsWrongTokenAndSurvivesAbort keeps the old generation
// serving after an unauthorized client and after a client that never commits.
func TestHandoverRejectsWrongTokenAndSurvivesAbort(t *testing.T) {
	dir := shortSocketDir(t)
	token := bytes.Repeat([]byte{0x24}, installationTokenBytes)
	path := HandoverPath(filepath.Join(dir, "h.sock"))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	start, _ := os.Open(os.DevNull)
	commit, _ := os.Open(os.DevNull)
	defer start.Close()
	defer commit.Close()
	done := make(chan error, 1)
	go func() {
		done <- ServeHandover(context.Background(), listener, HandoverConfig{UID: os.Geteuid(), Token: token, Start: []*os.File{start}, Commit: []*os.File{commit}})
	}()
	if _, _, err := DialHandover(context.Background(), path, os.Geteuid(), bytes.Repeat([]byte{0x25}, installationTokenBytes), 1); err == nil {
		t.Fatal("wrong token received the listeners")
	}
	aborted, _, err := DialHandover(context.Background(), path, os.Geteuid(), token, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = aborted.Close()
	select {
	case err := <-done:
		t.Fatalf("handover finished without a commit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	handover, _, err := DialHandover(context.Background(), path, os.Geteuid(), token, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handover.Commit(1); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, err := DialHandover(context.Background(), filepath.Join(dir, "missing"), os.Geteuid(), token, 1); !errors.Is(err, ErrNoHandover) {
		t.Fatalf("missing socket = %v, want ErrNoHandover", err)
	}
}

func shortSocketDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "pbho-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// hammer opens a fresh connection per request so every request exercises accept.
type hammer struct {
	mu       sync.Mutex
	owners   []string
	failures []error
	quit     chan struct{}
	done     chan struct{}
}

func startHammer(address string) *hammer {
	h := &hammer{quit: make(chan struct{}), done: make(chan struct{})}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	go func() {
		defer close(h.done)
		for {
			select {
			case <-h.quit:
				return
			default:
			}
			response, err := client.Get("http://" + address + "/")
			var owner []byte
			if err == nil {
				owner, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			h.mu.Lock()
			if err != nil {
				h.failures = append(h.failures, err)
			} else {
				h.owners = append(h.owners, string(owner))
			}
			h.mu.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return h
}

// lastOwners is the single owner of the last n requests, or "" when mixed.
func (h *hammer) lastOwners(n int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.owners) < n {
		return ""
	}
	tail := h.owners[len(h.owners)-n:]
	for _, owner := range tail {
		if owner != tail[0] {
			return ""
		}
	}
	return tail[0]
}

func (h *hammer) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.owners...)
}

func (h *hammer) stop() (map[string]int, []error) {
	close(h.quit)
	<-h.done
	counts := map[string]int{}
	for _, owner := range h.owners {
		counts[owner]++
	}
	return counts, h.failures
}

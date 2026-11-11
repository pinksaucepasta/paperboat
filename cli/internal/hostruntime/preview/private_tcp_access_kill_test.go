package preview

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// privateTCPAccessOwnerReady is printed once the helper process runs, so the test starts the session TTL after spawn.
const privateTCPAccessOwnerReady = "private-tcp-access-owner-ready"

type privateTCPAccessListenerProxy struct{ listener net.Listener }

func (p privateTCPAccessListenerProxy) Close() error { return p.listener.Close() }
func (p privateTCPAccessListenerProxy) AccessURL() string {
	return "http://" + p.listener.Addr().String()
}

// TestPrivateTCPAccessOwnerHelper is the "CLI" of the kill test: a real process that beats for a session until killed.
func TestPrivateTCPAccessOwnerHelper(t *testing.T) {
	target := os.Getenv("PB_TEST_ACCESS_HEARTBEAT_URL")
	if target == "" {
		t.Skip("helper process of TestPrivateTCPAccessListenerClosesWhenItsOwnerProcessIsKilled")
	}
	fmt.Println(privateTCPAccessOwnerReady)
	for {
		request, _ := http.NewRequest(http.MethodPut, target, nil)
		request.Header.Set("Authorization", "Bearer local-token")
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func dialsPrivateTCPAccess(address string) bool {
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err == nil {
		connection.Close()
	}
	return err == nil
}

func TestPrivateTCPAccessListenerClosesWhenItsOwnerProcessIsKilled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager, err := NewPrivateTCPAccessManager(PrivateTCPAccessManagerConfig{ControlToken: "local-token", RunContext: ctx, HeartbeatTTL: 600 * time.Millisecond,
		resolve: func(context.Context, string) (string, string, error) { return "route_tcp_1", "tun_1", nil },
		start: func(context.Context, PrivateTCPAccessRequest) (privateTCPAccessProxy, error) {
			return privateTCPAccessListenerProxy{listener}, nil
		},
		newID: func() (string, error) { return "kill", nil }})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = manager.Close() })
	server := httptest.NewServer(manager)
	t.Cleanup(server.Close)
	// Spawn the owner first: its beats get 404 until the session exists, and the TTL starts only once it runs.
	owner := exec.Command(os.Args[0], "-test.run=^TestPrivateTCPAccessOwnerHelper$")
	owner.Env = append(os.Environ(), "PB_TEST_ACCESS_HEARTBEAT_URL="+server.URL+"/v1/private-tcp-access/access_kill/heartbeat")
	output, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = owner.Process.Kill()
		}
		_ = owner.Wait()
	})
	ready := make(chan bool, 1) // the helper prints once, so nothing is left to drain
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			if scanner.Text() == privateTCPAccessOwnerReady {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("owner helper exited before it was ready")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("owner helper never became ready")
	}
	start, err := http.NewRequest(http.MethodPost, server.URL+"/v1/private-tcp-access", strings.NewReader(validPrivateTCPAccessBody("tun_1")))
	if err != nil {
		t.Fatal(err)
	}
	start.Header.Set("Authorization", "Bearer local-token")
	response, err := http.DefaultClient.Do(start)
	if err != nil {
		t.Fatal(err)
	}
	var session privateTCPAccessResponseDocument
	err = json.NewDecoder(response.Body).Decode(&session)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("start=%d %v", response.StatusCode, err)
	}
	address := listener.Addr().String()
	time.Sleep(1500 * time.Millisecond) // well past one TTL: only the live owner's beats keep the listener open
	if !dialsPrivateTCPAccess(address) {
		t.Fatal("a beating owner process lost its listener")
	}
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed = true
	_ = owner.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for dialsPrivateTCPAccess(address) {
		if time.Now().After(deadline) {
			t.Fatal("the listener of a killed owner process stayed open")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

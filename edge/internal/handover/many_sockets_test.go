package handover_test

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/handover"
)

// manySockets is how many public TCP listeners a node holds when every managed node binds every tunnel's port; a
// handover must pass them all (the protocol sends 200 sockets per message, below the kernel's SCM_MAX_FD of 253).
const manySockets = 2000

// runOldWithManySockets is the serving process of TestHandoverPassesTwoThousandTCPListeners: it listens on
// manySockets ports, writes their addresses to the state directory, offers them and waits to be released.
func runOldWithManySockets(dir string) {
	var probes []net.Listener
	var addresses []string
	for range manySockets {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
		probes = append(probes, l)
		addresses = append(addresses, l.Addr().String())
	}
	for _, l := range probes {
		_ = l.Close()
	}
	for _, address := range addresses {
		if _, err := handover.Listen("tcp", address); err != nil {
			panic(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "addresses"), []byte(strings.Join(addresses, "\n")), 0o600); err != nil {
		panic(err)
	}
	released := make(chan struct{})
	if _, err := handover.Offer(dir, "epoch_many", nil, func() { close(released) }); err != nil {
		panic(err)
	}
	os.Stdout.WriteString("ready\n")
	<-released
	time.Sleep(time.Second)
	os.Exit(0)
}

// A node that listens on every public tunnel port still hands over without a gap: 2,000 inherited TCP sockets pass
// from the serving process to its replacement, each listens (and accepts) on its own inherited socket.
func TestHandoverPassesTwoThousandTCPListeners(t *testing.T) {
	dir := t.TempDir()
	old := exec.Command(os.Args[0], "-test.run=^$")
	old.Env = append(os.Environ(), "PB_HANDOVER_MANY="+dir)
	stdout, err := old.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = old.Wait(); close(exited) }()
	defer func() { _ = old.Process.Kill(); <-exited }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("the serving process did not come up: %q %v", line, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "addresses"))
	if err != nil {
		t.Fatal(err)
	}
	addresses := strings.Fields(string(raw))
	if len(addresses) != manySockets {
		t.Fatalf("%d addresses", len(addresses))
	}
	inheritance, err := handover.Inherit(dir)
	if err != nil || inheritance == nil || inheritance.Epoch != "epoch_many" {
		t.Fatalf("inherit: %v %v", inheritance, err)
	}
	// The serving process still holds every port, so a listener that binds fresh would fail: success proves each
	// socket was inherited, not rebound.
	listeners := make([]net.Listener, len(addresses))
	for i, address := range addresses {
		if listeners[i], err = handover.Listen("tcp", address); err != nil {
			t.Fatalf("listener %d (%s) not inherited: %v", i, address, err)
		}
		defer listeners[i].Close()
	}
	ready, cancel := context.WithTimeout(context.Background(), loadedHostBudget)
	defer cancel()
	if err := inheritance.Ready(ready); err != nil {
		t.Fatal(err)
	}
	var accepting sync.WaitGroup
	for _, l := range listeners {
		accepting.Go(func() {
			if c, err := l.Accept(); err == nil {
				_ = c.Close()
			}
		})
	}
	var dialing sync.WaitGroup
	failures := make(chan string, len(addresses))
	for _, address := range addresses {
		dialing.Go(func() {
			c, err := net.DialTimeout("tcp", address, loadedHostBudget)
			if err != nil {
				failures <- address + ": " + err.Error()
				return
			}
			_ = c.Close()
		})
	}
	dialing.Wait()
	close(failures)
	for failure := range failures {
		t.Errorf("dial failed: %s", failure)
	}
	accepting.Wait()
}

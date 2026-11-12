package handover_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/tunnel/internal/handover"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/runtime"
)

const tcpAddress, udpAddress = "127.0.0.1:0", "127.0.0.1:0"

// The serving ("old") process of TestHandoverServesEveryRequestAcrossTheSwitch: it serves "old"
// until a replacement is ready, then drains like the edge does and exits.
func runOld(dir string) {
	l, err := handover.Listen("tcp", tcpAddress)
	if err != nil {
		panic(err)
	}
	udp, err := handover.ListenPacket("udp", udpAddress)
	if err != nil {
		panic(err)
	}
	var drainer runtime.Drainer
	server := &http.Server{Handler: drainer.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "old") }))}
	go func() { _ = server.Serve(l) }()
	released := make(chan struct{})
	if _, err := handover.Offer(dir, "epoch_handover", nil, func() { close(released) }); err != nil {
		panic(err)
	}
	os.Stdout.WriteString(l.Addr().String() + " " + udp.LocalAddr().String() + "\n")
	<-released
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = drainer.Drain(ctx, server, l, 3*time.Second) // the edge's drain (runtime.HTTPServer.Drain)
	time.Sleep(500 * time.Millisecond)               // the drain: UDP stays this process's until it exits
	_ = udp.Close()
	os.Exit(0)
}

func TestMain(m *testing.M) {
	if dir := os.Getenv("PB_HANDOVER_OLD"); dir != "" {
		runOld(dir)
	}
	os.Exit(m.Run())
}

// An edge install starts the new process on the old one's sockets: every request through the
// switch is answered (by the old process, then the new one), nothing is refused, the epoch
// carries over, and the new process reads UDP only after the old one exited.
func TestHandoverServesEveryRequestAcrossTheSwitch(t *testing.T) {
	dir := t.TempDir()
	old := exec.Command(os.Args[0], "-test.run=^$")
	old.Env = append(os.Environ(), "PB_HANDOVER_OLD="+dir)
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
	if err != nil {
		t.Fatal(err)
	}
	addresses := strings.Fields(line)

	var failures, fromNew atomic.Int64
	var answers sync.Map
	stop := make(chan struct{})
	var clients sync.WaitGroup
	for range 4 {
		clients.Add(1)
		go func() {
			defer clients.Done()
			client := &http.Client{Timeout: 5 * time.Second}
			for {
				select {
				case <-stop:
					return
				default:
				}
				response, err := client.Get("http://" + addresses[0] + "/")
				if err != nil {
					failures.Add(1)
					t.Log(err)
					continue
				}
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				answers.Store(string(body), true)
				if string(body) == "new" {
					fromNew.Add(1)
				}
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)

	inheritance, err := handover.Inherit(dir)
	if err != nil || inheritance == nil {
		t.Fatalf("inherit: %v", err)
	}
	if inheritance.Epoch != "epoch_handover" {
		t.Fatalf("epoch = %q", inheritance.Epoch)
	}
	l, err := handover.Listen("tcp", tcpAddress)
	if err != nil {
		t.Fatal(err)
	}
	if l.Addr().String() != addresses[0] {
		t.Fatalf("listener %s is not the inherited %s", l.Addr(), addresses[0])
	}
	udp, err := handover.ListenPacket("udp", udpAddress)
	if err != nil || udp.LocalAddr().String() != addresses[1] {
		t.Fatalf("UDP socket not inherited: %v %v", udp, err)
	}
	if _, ok := udp.(interface {
		ReadMsgUDP(b, oob []byte) (int, int, int, *net.UDPAddr, error)
	}); !ok {
		t.Fatal("inherited UDP socket lost *net.UDPConn's methods")
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "new") })}
	go func() { _ = server.Serve(l) }()
	defer server.Close()
	time.Sleep(200 * time.Millisecond) // warm-up: the old process still answers everything
	if fromNew.Load() != 0 {
		t.Fatal("the new process accepted before it was ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := inheritance.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handover.UDPReady():
		t.Fatal("UDP handed over while the old process still runs")
	default:
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("old process did not drain and exit")
	}
	select {
	case <-handover.UDPReady():
	case <-time.After(5 * time.Second):
		t.Fatal("UDP not handed over after the old process exited")
	}
	before := fromNew.Load()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	clients.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d requests failed across the handover", failures.Load())
	}
	if _, ok := answers.Load("old"); !ok {
		t.Fatal("the old process never answered")
	}
	if fromNew.Load() <= before {
		t.Fatal("the new process does not answer after the old one exited")
	}
}

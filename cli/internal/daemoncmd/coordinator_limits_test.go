package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/daemonrpc"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
	"github.com/pinksaucepasta/paperboat/cli/internal/testcert"
)

// countingLimitsClient hands out listeners that bind nothing and records what the guard was asked for.
type countingLimitsClient struct {
	*coordinatorNameClient
	listeners, names, aliases int
	refuseAliases             bool
}

func (c *countingLimitsClient) AcquireMachine(_ context.Context, _, _, _ string, ip netip.Addr, _ int) (net.Listener, netip.Addr, error) {
	c.listeners++
	return newIdleListener(), ip, nil
}

func (c *countingLimitsClient) ReplaceAliases(ctx context.Context, aliases map[string]string) error {
	if c.refuseAliases && len(aliases) > 0 {
		return machineguard.ErrNamesRefused
	}
	c.aliases = len(aliases)
	return c.coordinatorNameClient.ReplaceAliases(ctx, aliases)
}

func (c *countingLimitsClient) ReplaceNames(ctx context.Context, names map[string]netip.Addr) error {
	c.names = len(names)
	return c.coordinatorNameClient.ReplaceNames(ctx, names)
}

type idleListener struct{ closed chan struct{} }

func newIdleListener() *idleListener { return &idleListener{closed: make(chan struct{})} }
func (l *idleListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}
func (l *idleListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}
func (*idleListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func limitPeers(n int, ports ...int32) []*daemonrpc.PeerUpdate {
	var peers []*daemonrpc.PeerUpdate
	for i := range n {
		peers = append(peers, &daemonrpc.PeerUpdate{PeerId: fmt.Sprintf("m%05d", i), Alias: fmt.Sprint("b", i), AssignedIp: fmt.Sprintf("127.100.%d.%d", 1+i/250, 1+i%250), Approved: true, Online: true, ExportedPorts: ports})
	}
	return peers
}

func startLimitsCoordinator(t *testing.T, client guardedNameClient) *Coordinator {
	t.Helper()
	ca, err := testcert.New()
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.IssueCertificate([]string{"*." + testBrowserDomain})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(CoordinatorConfig{SocketAddress: socketAddress(shorttemp.Dir(t)), RetryBase: time.Hour, RetryMax: time.Hour,
		ConnectNameClient: func(context.Context) (guardedNameClient, error) { return client, nil },
		DialMachine:       func(context.Context, string, int) (net.Conn, error) { return nil, errors.New("no") },
		IssueCertificate:  func(context.Context, string, string, int) (tls.Certificate, error) { return pair, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c
}

// An account with more than 128 routed machines lost its whole local access setup.
func TestCoordinatorServesEveryMachineUpToTheCeilingAndNamesTheRest(t *testing.T) {
	for _, n := range []int{129, 179, 512, 513, 1000, machineguard.MaxRoutedMachines, machineguard.MaxRoutedMachines + 1} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			client := &countingLimitsClient{coordinatorNameClient: &coordinatorNameClient{}}
			c := startLimitsCoordinator(t, client)
			started := time.Now()
			if err := c.reconcileSnapshot(t.Context(), limitPeers(n, 22, 80), 1); err != nil {
				t.Fatal(err)
			}
			t.Logf("%d machines: %s", n, time.Since(started).Round(time.Millisecond))
			served := min(n, machineguard.MaxRoutedMachines)
			if client.names != served || client.listeners != 2*served || client.aliases != 3*served { // 22, 80 and the wildcard
				t.Fatalf("names=%d listeners=%d aliases=%d, want %d machines", client.names, client.listeners, client.aliases, served)
			}
			step, detail := c.PendingLocalAccessStep(), c.PendingLocalAccessDetail()
			if n == served && step != "" {
				t.Fatalf("pending step %q %q", step, detail)
			}
			if n > served && (step != "machine_limit" || detail != "1 machines") {
				t.Fatalf("pending step %q %q", step, detail)
			}
			// The same machines stay served whatever order the next snapshot has, and shrinking clears the notice.
			if err := c.reconcileSnapshot(t.Context(), limitPeers(served, 22, 80), 2); err != nil {
				t.Fatal(err)
			}
			if client.listeners != 2*served || c.PendingLocalAccessStep() != "" {
				t.Fatalf("second snapshot: listeners=%d step=%q", client.listeners, c.PendingLocalAccessStep())
			}
		})
	}
}

func TestLimitsShareListenersAndBoundAliases(t *testing.T) {
	ports := make([]int32, 65535)
	for i := range ports {
		ports[i] = int32(i + 1)
	}
	peers := append(limitPeers(1000, 22, 80), limitPeers(1001, ports...)[1000])
	kept, machines, dropped := routableWithinLimits(peers)
	total := 0
	for _, peer := range kept {
		total += len(peer.GetExportedPorts())
	}
	if machines != 0 || len(kept) != 1001 || total != machineguard.MaxMachineListeners || dropped != 2000+65535-total {
		t.Fatalf("kept %d machines %d ports, dropped %d machines %d ports", len(kept), total, machines, dropped)
	}
	if len(kept[0].GetExportedPorts()) != 2 || len(peers[1000].GetExportedPorts()) != 65535 {
		t.Fatal("a machine with many ports took another's, or the snapshot was changed")
	}
	set := browserAliasSet{names: map[string]string{}}
	for i := range machineguard.MaxBrowserAliases + 5 {
		set.add(fmt.Sprint(i), "gateway")
	}
	if len(set.names) != machineguard.MaxBrowserAliases || set.skipped != 5 {
		t.Fatalf("%d aliases, %d skipped", len(set.names), set.skipped)
	}
}

func TestCoordinatorKeepsMachineAccessWhenTheGuardRefusesBrowserNames(t *testing.T) {
	client := &countingLimitsClient{coordinatorNameClient: &coordinatorNameClient{}, refuseAliases: true}
	c := startLimitsCoordinator(t, client)
	if err := c.reconcileSnapshot(t.Context(), limitPeers(3, 22, 80), 1); err != nil {
		t.Fatal(err)
	}
	if client.names != 3 || client.listeners != 6 || c.PendingLocalAccessStep() != "machine_names" {
		t.Fatalf("names=%d listeners=%d step=%q", client.names, client.listeners, c.PendingLocalAccessStep())
	}
}

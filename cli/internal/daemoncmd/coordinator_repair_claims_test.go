package daemoncmd

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// orderNameClient records the order of guard operations: claim sets, machine acquires and listener closes.
type orderNameClient struct {
	*coordinatorNameClient
	order  sync.Mutex
	events []string
}

func (c *orderNameClient) record(event string) {
	c.order.Lock()
	c.events = append(c.events, event)
	c.order.Unlock()
}

func (c *orderNameClient) AcquireMachine(ctx context.Context, account, machine, hostname string, ip netip.Addr, port int) (net.Listener, netip.Addr, error) {
	c.record("acquire " + machine)
	listener, assigned, err := c.coordinatorNameClient.AcquireMachine(ctx, account, machine, hostname, ip, port)
	if err != nil {
		return nil, assigned, err
	}
	return &closeRecordingListener{Listener: listener, close: func() { c.record("close " + machine) }}, assigned, nil
}

func (c *orderNameClient) ReplaceClaims(ctx context.Context, account string, machines []string) error {
	c.record("claims " + strings.Join(machines, ","))
	return c.coordinatorNameClient.ReplaceClaims(ctx, account, machines)
}

type closeRecordingListener struct {
	net.Listener
	once  sync.Once
	close func()
}

func (l *closeRecordingListener) Close() error {
	l.once.Do(l.close)
	return l.Listener.Close()
}

// pb reset re-pairs a machine as a new record at the old record's server address. The old record's listeners
// close and its claim is released before the new record acquires, so the guard gives the new record the address
// pb machine list shows; acquiring first left the old claim active and the new record got another address.
func TestCoordinatorRepairedMachineReleasesTheOldRecordBeforeAcquiring(t *testing.T) {
	names := &orderNameClient{coordinatorNameClient: &coordinatorNameClient{}}
	c := startClaimsCoordinator(t, names)
	apply := func(id string) {
		t.Helper()
		machines := []api.UserMachine{{ID: id, Alias: "target", AssignedIP: "127.100.0.29", State: "online", Online: true, NetworkApproved: true, InstallationGeneration: 1}}
		services := []api.MachineServicesMachine{{MachineID: id, InstallationGeneration: 1, ExpiresAt: time.Now().Add(time.Hour), Services: []api.MachineServicePort{{Port: 45679, AddressFamily: "ipv4"}}}}
		if err := c.ApplyMachines(t.Context(), machines, services); err != nil {
			t.Fatal(err)
		}
	}
	apply("m_old")
	names.order.Lock()
	names.events = nil
	names.order.Unlock()
	apply("m_new")
	names.order.Lock()
	events := slices.Clone(names.events)
	names.order.Unlock()
	acquire := slices.Index(events, "acquire m_new")
	closed := slices.Index(events, "close m_old")
	released := slices.Index(events, "claims m_new")
	if acquire < 0 || closed < 0 || released < 0 || closed > acquire || released > acquire {
		t.Fatalf("guard operations %q: the old record's listener must close and its claim be released before the new record acquires", events)
	}
}

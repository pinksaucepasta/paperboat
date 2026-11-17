package machineguard

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

// heldListener stands for the guard's gateway listener: it reports the port it really holds.
type heldListener struct {
	net.Listener
	port int
}

func (l heldListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(splitdns.BrowserGatewayIP), Port: l.port}
}

func gatewayLease(port, held int) *guardedLease {
	gateway := &sharedGateway{port: port, listener: heldListener{port: held}}
	return &guardedLease{ip: splitdns.BrowserGatewayIP, port: port, uid: "1000", listener: &gatewayRoute{gateway: gateway}}
}

// A gateway port a web server holds on every address is redirected by a nat rule to the high port the guard
// listens on; the filter chains then name that high port. Ports the guard holds itself get no rule.
func TestProtectionRulesRedirectOnlyAHeldGatewayPort(t *testing.T) {
	rules, err := protectionRules([]*guardedLease{gatewayLease(80, 34080), gatewayLease(443, 443), gatewayLease(80, 34080)})
	if err != nil {
		t.Fatal(err)
	}
	gateway := splitdns.BrowserGatewayIP
	for _, want := range []string{
		"chain gateway_nat { type nat hook output priority -100;",
		"ip daddr " + gateway + " tcp dport 80 dnat ip to " + gateway + ":34080\n",
		"ip daddr " + gateway + " tcp dport 34080 accept\n",
		"ip daddr " + gateway + " tcp dport 34080 tcp flags & (syn | ack) == syn socket mark",
		"ip daddr " + gateway + " tcp dport 443 accept\n",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
	for _, unwanted := range []string{"dport 443 dnat", "dport 80 accept", "dport 80 tcp flags"} {
		if strings.Contains(rules, unwanted) {
			t.Errorf("rules contain %q:\n%s", unwanted, rules)
		}
	}
	if strings.Count(rules, "dport 80 dnat") != 1 {
		t.Errorf("two uids on one port must share one nat rule:\n%s", rules)
	}
}

// A machine port listens on a high port so a program can still bind 0.0.0.0 on the same port: the nat rule sends
// ip:port there and the uid rule names the high port the filter chain sees after the rewrite.
func TestProtectionRulesRedirectAMachinePortToItsHighPort(t *testing.T) {
	lease := &guardedLease{ip: "127.100.1.2", port: 8080, uid: "1000", listener: heldListener{port: 41234}}
	rules, err := protectionRules([]*guardedLease{lease})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ip daddr 127.100.1.2 tcp dport 8080 dnat ip to 127.100.1.2:41234\n",
		"ip daddr 127.100.1.2 tcp dport 41234 meta skuid 1000 accept\n",
		"ip daddr 127.100.1.2 tcp dport 41234 tcp flags & (syn | ack) == syn socket mark",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "dport 8080 meta skuid") {
		t.Errorf("uid rule names the dialed port, which the filter never sees after the nat rewrite:\n%s", rules)
	}
}

func TestProtectionRulesHaveNoNatChainWithoutARedirect(t *testing.T) {
	for _, leases := range [][]*guardedLease{nil, {gatewayLease(80, 80), {ip: "127.100.1.2", port: 22, uid: "1000"}}} {
		rules, err := protectionRules(leases)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(rules, "nat") || strings.Contains(rules, "dnat") {
			t.Errorf("rules without a held port carry a nat rule:\n%s", rules)
		}
	}
}

// The client socket still names the port it dialed (80), while the guard accepts on the high port: the owner
// lookup must use the dialed port, or every redirected connection is refused as unattributable.
func TestAttributionUsesTheDialedPortOfARedirectedGateway(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	var asked socketTuple
	lookup := func(tuple socketTuple) ([]socketOwner, error) {
		asked = tuple
		return []socketOwner{{Tuple: tuple, UID: 1000, Established: true}}, nil
	}
	own := func(uint32) (string, bool) { return "daemon", true }
	if _, _, err := attributeConnection(accepted, 80, lookup, own); err != nil {
		t.Fatal(err)
	}
	if asked.Destination.Port() != 80 || int(asked.Source.Port()) != client.LocalAddr().(*net.TCPAddr).Port {
		t.Fatalf("looked up %v, want the client socket dialing port 80", asked)
	}
}

// As root (the guard's own privilege; SO_MARK needs it): the gateway listens on a free high port, so a port held on
// every address still gets a gateway, and the lease reports the high port for the nat rule.
func TestRouteGatewayFallsBackWhenThePortIsHeld(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for the guard's SO_MARK listener")
	}
	holder, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	held := holder.Addr().(*net.TCPAddr).Port
	g := &guardServer{}
	route, err := g.routeGateway(t.Context(), "1000", held)
	if err != nil {
		t.Fatalf("held gateway port was not worked around: %v", err)
	}
	defer shutdownListener(route)
	if got := route.gateway.listenPort(); got == held || got == 0 {
		t.Fatalf("gateway listens on %d, want a free port other than %d", got, held)
	}
	if route.Addr().(*net.TCPAddr).Port != held {
		t.Fatal("the route must keep reporting the port clients dial")
	}
}

// The real nft parser accepts the generated table, nat chain included (a chain once named "redirect" broke
// the guard on a device: it is an nft keyword). Needs root for nft's netlink check.
func TestProtectionRulesParseInNft(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil || os.Geteuid() != 0 {
		t.Skip("needs nft and root")
	}
	rules, err := protectionRules([]*guardedLease{gatewayLease(80, 34080), gatewayLease(443, 443), {ip: "127.100.1.2", port: 22, uid: "1000"}})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "nft", "-c", "-f", "-")
	command.Stdin = strings.NewReader(rules)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nft rejects the rules: %v: %s\n%s", err, output, rules)
	}
}

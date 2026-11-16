//go:build darwin

package machineguard

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// TestDarwinRulesLetAnyUIDReachTheRoutedGateway: the routed gateway accepts every uid (attribution then refuses
// users without a daemon with the refusal page, F23-02); a machine address stays limited to its owner's uid.
func TestDarwinRulesLetAnyUIDReachTheRoutedGateway(t *testing.T) {
	restore := darwinBrokerIdentity
	darwinBrokerIdentity = func(context.Context) (uint32, uint32, error) { return 70, 70, nil }
	t.Cleanup(func() { darwinBrokerIdentity = restore })
	rules, err := darwinRules(t.Context(), []*guardedLease{
		{ip: splitdns.BrowserGatewayIP, port: 443, uid: "501"},
		{ip: "127.100.0.4", port: 18765, uid: "502"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pass out quick on lo0 inet proto tcp from any to " + splitdns.BrowserGatewayIP + " port 443 no state",
		"pass in quick on lo0 inet proto tcp from " + splitdns.BrowserGatewayIP + " port 443 to any no state",
		"pass out quick on lo0 inet proto tcp from any to 127.100.0.4 port 18765 user 502 no state",
		"pass in quick on lo0 inet proto tcp from any to " + splitdns.BrowserGatewayIP + " port 443 user 70 flags S/SA keep state",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("rules lack %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, splitdns.BrowserGatewayIP+" port 443 user 501") {
		t.Fatalf("the routed gateway is still limited to its first user:\n%s", rules)
	}
}

// A machine port listens on a high port so a program can still bind 0.0.0.0 on the same port: rdr sends ip:port
// there (translation before filter rules) and the broker's rules name the high port pf sees after the rewrite.
func TestDarwinRulesRedirectAMachinePortToItsHighPort(t *testing.T) {
	restore := darwinBrokerIdentity
	darwinBrokerIdentity = func(context.Context) (uint32, uint32, error) { return 70, 70, nil }
	t.Cleanup(func() { darwinBrokerIdentity = restore })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	high := listener.Addr().(*net.TCPAddr).Port
	rules, err := darwinRules(t.Context(), []*guardedLease{{ip: "127.100.0.4", port: 8080, uid: "502", listener: listener}})
	if err != nil {
		t.Fatal(err)
	}
	rdr := fmt.Sprintf("rdr on lo0 inet proto tcp from any to 127.100.0.4 port 8080 -> 127.100.0.4 port %d\n", high)
	for _, want := range []string{
		rdr,
		"pass out quick on lo0 inet proto tcp from any to 127.100.0.4 port 8080 user 502 no state",
		fmt.Sprintf("pass in quick on lo0 inet proto tcp from any to 127.100.0.4 port %d user 70 flags S/SA keep state", high),
		fmt.Sprintf("pass out quick on lo0 inet proto tcp from 127.100.0.4 port %d to any user 70 no state", high),
		"pass in quick on lo0 inet proto tcp from 127.100.0.4 port 8080 to any user 502 no state",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
	if !strings.HasPrefix(rules, rdr) { // pfctl refuses translation rules after filter rules
		t.Errorf("translation rules must come before filter rules:\n%s", rules)
	}
}

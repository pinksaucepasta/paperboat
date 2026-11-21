package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

func tunnelConfig() selfhost.Config {
	return selfhost.Config{Listen: "[::]:7277", Components: []selfhost.Component{{Capability: "tunnel", TCPPort: 7272, QUICPort: 7273}}}
}

func doctorFixture(t *testing.T, outgoing string, answers map[string]string, probe containers.PortProbe, probeErr error) (doctorDeps, *strings.Builder) {
	f := newFakeHost(t, outgoing)
	f.answers = answers
	deps := doctorDeps{
		run:    f.run,
		paths:  f.paths,
		probe:  func(context.Context, string, int, string) (containers.PortProbe, error) { return probe, probeErr },
		listen: func(string) (int, func(), error) { return 25000, func() {}, nil },
		ingress: func(context.Context, string) (containers.IngressState, error) {
			return containers.IngressState{Verified: true}, nil
		},
	}
	return deps, &strings.Builder{}
}

func TestDoctorPassesWhenEverythingIsOpen(t *testing.T) {
	deps, out := doctorFixture(t, "60000 65535", map[string]string{"nft list table inet " + edgeGuardTable: ""}, containers.PortProbe{Reachable: true, Detail: "connected"}, nil)
	if err := runDoctor(t.Context(), tunnelConfig(), deps, out); err != nil {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if strings.Contains(out.String(), "FIX") {
		t.Fatalf("unexpected FIX:\n%s", out)
	}
}

func TestDoctorNamesTheMissingRule(t *testing.T) {
	for detail, want := range map[string]string{
		"timeout":     "cloud firewall",
		"unreachable": "this host's firewall rejects it",
		"refused":     "nothing listened",
	} {
		deps, out := doctorFixture(t, "60000 65535", nil, containers.PortProbe{Detail: detail}, nil)
		err := runDoctor(t.Context(), tunnelConfig(), deps, out)
		var command commandError
		if !errors.As(err, &command) || !strings.Contains(out.String(), want) {
			t.Errorf("%s: err %v, output:\n%s", detail, err, out)
		}
		if detail == "timeout" && !strings.Contains(out.String(), "10000-59999") {
			t.Errorf("timeout advice does not print the cloud rule:\n%s", out)
		}
	}
}

func TestDoctorReportsAWrongOutgoingRangeAndAClosedHostFirewall(t *testing.T) {
	answers := map[string]string{"iptables -S INPUT": "-P INPUT ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"} // rule missing: -C fails
	deps, out := doctorFixture(t, "32768 60999", answers, containers.PortProbe{Reachable: true, Detail: "connected"}, nil)
	if err := runDoctor(t.Context(), tunnelConfig(), deps, out); err == nil {
		t.Fatalf("doctor passed:\n%s", out)
	}
	for _, want := range []string{"outgoing port range is \"32768 60999\"", "iptables host firewall does not allow TCP 7277, 443, 7272, 10000-59999 and UDP 443, 7273"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorReportsAFailedProbeCall(t *testing.T) {
	deps, out := doctorFixture(t, "60000 65535", nil, containers.PortProbe{}, errors.New("HTTP 503"))
	if err := runDoctor(t.Context(), tunnelConfig(), deps, out); err == nil || !strings.Contains(out.String(), "could not run the reachability check") {
		t.Fatalf("err %v output:\n%s", err, out)
	}
}

func TestDoctorSkipsRelayOnlyInstallations(t *testing.T) {
	deps, out := doctorFixture(t, "32768 60999", nil, containers.PortProbe{}, nil)
	deps.probe = func(context.Context, string, int, string) (containers.PortProbe, error) {
		t.Fatal("probed a relay-only node")
		return containers.PortProbe{}, nil
	}
	if err := runDoctor(t.Context(), selfhost.Config{Components: []selfhost.Component{{Capability: "relay"}}}, deps, out); err != nil {
		t.Fatal(err)
	}
}

func TestTestListenerEchoesTheNonceInsideThePublicRange(t *testing.T) {
	port, stop, err := listenTestPort("the-nonce-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if !edgeports.PublicTCP(port) {
		t.Fatalf("port %d outside the public range", port)
	}
	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	got, _ := io.ReadAll(connection)
	if string(got) != "the-nonce-0123456789" {
		t.Fatalf("echo = %q", got)
	}
}

func TestDoctorReportsEndpointHostVerificationWithTheFix(t *testing.T) {
	config := tunnelConfig()
	config.EndpointHost = "edge.example.test"
	for reason, want := range map[string]string{
		"unresolved":        "add an A/AAAA record",
		"unreachable":       "open TCP 443",
		"identity_mismatch": "points at another server",
		"pending":           "wait a minute",
	} {
		deps, out := doctorFixture(t, "60000 65535", map[string]string{"nft list table inet " + edgeGuardTable: ""}, containers.PortProbe{Reachable: true, Detail: "connected"}, nil)
		deps.ingress = func(context.Context, string) (containers.IngressState, error) {
			return containers.IngressState{Reason: reason}, nil
		}
		if err := runDoctor(t.Context(), config, deps, out); err == nil {
			t.Fatalf("%s: an unverified endpoint host passed doctor:\n%s", reason, out)
		}
		if !strings.Contains(out.String(), "FIX  the control plane has not verified edge.example.test") || !strings.Contains(out.String(), want) {
			t.Fatalf("%s: advice lacks %q:\n%s", reason, want, out)
		}
	}
	deps, out := doctorFixture(t, "60000 65535", map[string]string{"nft list table inet " + edgeGuardTable: ""}, containers.PortProbe{Reachable: true, Detail: "connected"}, nil)
	if err := runDoctor(t.Context(), config, deps, out); err != nil || !strings.Contains(out.String(), "ok   the control plane verified edge.example.test reaches this node") {
		t.Fatalf("verified endpoint host not reported: %v\n%s", err, out)
	}
}

func TestDoctorChecksEveryNodePort(t *testing.T) {
	c := selfhost.Config{Components: []selfhost.Component{
		{Capability: "tunnel", TCPPort: edgeports.CarrierTCP, QUICPort: edgeports.CarrierQUIC},
		{Capability: "relay", TCPPort: edgeports.RelayWSS, QUICPort: edgeports.RelayQUIC},
	}}
	for _, test := range []struct {
		results map[string]string // "tcp 7272" -> detail; missing means connected
		want    []string
	}{
		{nil, []string{"ok   TCP 7272 (tunnel) is reachable", "ok   TCP 7274 (relay) is reachable", "ok   UDP 7274 (relay STUN) is reachable", "skip UDP 7273 (tunnel QUIC): the control plane does not send QUIC; make sure inbound UDP 7273 is allowed", "skip UDP 7275 (relay QUIC)"}},
		{map[string]string{"tcp 7272": "timeout"}, []string{"FIX  TCP 7272 (tunnel) is not reachable", "allow inbound TCP 7272 from anywhere"}},
		{map[string]string{"udp 7274": "timeout"}, []string{"allow inbound UDP 7274 from anywhere"}},
		{map[string]string{"tcp 7274": "refused"}, []string{"FIX  TCP 7274 (relay) is not reachable", "connection refused", "run pbh status and check the relay service"}},
		{map[string]string{"tcp 7272": "unreachable"}, []string{"rejected by the network (unreachable)", "host firewall check above"}},
	} {
		deps, out := doctorFixture(t, "60000 65535", map[string]string{"nft list table inet " + edgeGuardTable: ""}, containers.PortProbe{}, nil)
		var probed []string
		deps.probe = func(_ context.Context, transport string, port int, nonce string) (containers.PortProbe, error) {
			key := transport + " " + strconv.Itoa(port)
			probed = append(probed, key)
			if (port == 25000) != (nonce != "") {
				t.Errorf("%s: nonce %q", key, nonce)
			}
			if detail, ok := test.results[key]; ok {
				return containers.PortProbe{Detail: detail}, nil
			}
			return containers.PortProbe{Reachable: true, Detail: "connected"}, nil
		}
		err := runDoctor(t.Context(), c, deps, out)
		if (err == nil) != (test.results == nil) {
			t.Errorf("%v: err %v\n%s", test.results, err, out)
		}
		for _, want := range test.want {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if got := strings.Join(probed, ","); got != "tcp 25000,tcp 7272,tcp 7274,udp 7274" {
			t.Errorf("probed %s", got)
		}
	}
}

func TestNodePortsSkipsWhatTheProbeDoesNotAccept(t *testing.T) {
	var got []string
	for _, p := range nodePorts(selfhost.Component{Capability: "relay", TCPPort: 443, QUICPort: 7275}) {
		got = append(got, fmt.Sprintf("%s %d %s skip=%v", p.transport, p.port, p.service, p.skip != ""))
	}
	if want := "tcp 443 relay skip=true,udp 443 relay STUN skip=true,udp 7275 relay QUIC skip=true"; strings.Join(got, ",") != want {
		t.Fatalf("got %s\nwant %s", strings.Join(got, ","), want)
	}
}

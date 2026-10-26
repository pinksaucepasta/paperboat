package main

import (
	"context"
	"errors"
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
	return selfhost.Config{Components: []selfhost.Component{{Capability: "tunnel"}}}
}

func doctorFixture(t *testing.T, outgoing string, answers map[string]string, probe containers.PortProbe, probeErr error) (doctorDeps, *strings.Builder) {
	f := newFakeHost(t, outgoing)
	f.answers = answers
	deps := doctorDeps{
		run:    f.run,
		paths:  f.paths,
		probe:  func(context.Context, int, string) (containers.PortProbe, error) { return probe, probeErr },
		listen: func(string) (int, func(), error) { return 25000, func() {}, nil },
	}
	return deps, &strings.Builder{}
}

func TestDoctorPassesWhenEverythingIsOpen(t *testing.T) {
	deps, out := doctorFixture(t, "60000 65535", nil, containers.PortProbe{Reachable: true, Detail: "connected"}, nil)
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
	for _, want := range []string{"outgoing port range is \"32768 60999\"", "iptables host firewall does not allow TCP 443"} {
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
	deps.probe = func(context.Context, int, string) (containers.PortProbe, error) {
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

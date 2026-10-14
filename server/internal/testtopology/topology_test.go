//go:build topology

package testtopology

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	docker "github.com/ory/dockertest/v3/docker"
)

const topologyGoImage = "golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b"

const topologyNetToolsImage = "nicolaka/netshoot@sha256:a20c2531bf35436ed3766cd6cfe89d352b050ccc4d7005ce6400adf97503da1b"

func TestSplitDigestImageRequiresExactSHA256(t *testing.T) {
	repository, tag, err := splitDigestImage(topologyGoImage)
	if err != nil || repository != "golang:1.27.1-bookworm@sha256" || len(tag) != 64 {
		t.Fatalf("split = %q %q %v", repository, tag, err)
	}
	repository, tag, err = splitDigestImage(topologyNetToolsImage)
	if err != nil || repository != "nicolaka/netshoot@sha256" || len(tag) != 64 {
		t.Fatalf("network tools split = %q %q %v", repository, tag, err)
	}
	for _, invalid := range []string{"", "golang:latest", "golang@sha256:short", topologyGoImage + " extra", "@sha256:" + strings.Repeat("a", 64)} {
		if _, _, err := splitDigestImage(invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("image %q accepted", invalid)
		}
	}
}

func TestRunIDsAndRolesAreBounded(t *testing.T) {
	first, err := newRunID("paperboat")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newRunID("paperboat")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) != len("paperboat-")+20 || !strings.HasPrefix(first, "paperboat-") {
		t.Fatalf("run IDs = %q %q", first, second)
	}
	for _, role := range []string{"client", "host-1", "postgres"} {
		if !validRole(role) {
			t.Fatalf("valid role %q rejected", role)
		}
	}
	for _, role := range []string{"", "UPPER", "-bad", strings.Repeat("a", 49), "bad_role"} {
		if validRole(role) {
			t.Fatalf("invalid role %q accepted", role)
		}
	}
}

func TestClearingInactiveNetworkImpairmentIsNoop(t *testing.T) {
	scope := &Scope{
		containers: map[string]*dockertest.Resource{"client": {}},
		faults:     make(map[string]faultState),
		mu:         sync.Mutex{},
	}
	if err := scope.SetNetworkImpairment("client", 0, 0); err != nil {
		t.Fatal(err)
	}
	if len(scope.Evidence().Events) != 0 {
		t.Fatal("no-op impairment clear recorded an event")
	}
}

func TestBoundedLogsAndResourceValidators(t *testing.T) {
	buffer := newBoundedBuffer(4)
	if written, err := buffer.Write([]byte("abcdef")); err != nil || written != 6 || string(buffer.Bytes()) != "abcd" || !buffer.truncated {
		t.Fatalf("bounded write = %d %q %t %v", written, buffer.Bytes(), buffer.truncated, err)
	}
	for _, port := range []string{"1/tcp", "443/udp", "65535/tcp"} {
		if !validPort(port) {
			t.Fatalf("valid port %q rejected", port)
		}
	}
	for _, port := range []string{"0/tcp", "65536/tcp", "80", "80/sctp", " 80/tcp"} {
		if validPort(port) {
			t.Fatalf("invalid port %q accepted", port)
		}
	}
	for _, path := range []string{"/data", "/var/lib/paperboat"} {
		if !validContainerPath(path) {
			t.Fatalf("valid path %q rejected", path)
		}
	}
	for _, path := range []string{"", "/", "relative", "/data:ro", "/data/../secret"} {
		if validContainerPath(path) {
			t.Fatalf("invalid path %q accepted", path)
		}
	}
}

func TestConcurrentScopesOwnAndCleanOnlyTheirResources(t *testing.T) {
	if os.Getenv("PAPERBOAT_TOPOLOGY_TEST") != "1" {
		t.Skip("set PAPERBOAT_TOPOLOGY_TEST=1 to run Docker topology isolation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	first := newIntegrationScope(t, ctx, "pbtopology-a")
	second := newIntegrationScope(t, ctx, "pbtopology-b")
	firstClosed, secondClosed := false, false
	t.Cleanup(func() {
		if !firstClosed {
			_, _ = first.Close(true)
		}
		if !secondClosed {
			_, _ = second.Close(true)
		}
	})

	for _, scope := range []*Scope{first, second} {
		if err := scope.CreateNetwork(ctx, "client", true); err != nil {
			t.Fatal(err)
		}
		if err := scope.CreateNetwork(ctx, "control", false); err != nil {
			t.Fatal(err)
		}
		if err := scope.CreateVolume(ctx, "state"); err != nil {
			t.Fatal(err)
		}
		if err := scope.RunContainer(ctx, ContainerSpec{
			Role: "client", Image: topologyGoImage, Command: []string{"sh", "-c", "printf '%s' \"$PAPERBOAT_MARKER\" > /state/marker; echo topology-ready; echo topology-diagnostic >&2; while :; do sleep 60; done"},
			Environment: []string{"PAPERBOAT_MARKER=" + scope.RunID()}, Networks: []string{"client", "control"},
			Volumes: []VolumeMount{{Role: "state", ContainerPath: "/state"}}, PublishPorts: []string{"8080/tcp"},
			MemoryBytes: 64 << 20, CPUQuota: 25000, PIDs: 32,
		}); err != nil {
			t.Fatal(err)
		}
		hostPort, err := scope.HostPort("client", "8080/tcp")
		if err != nil || !strings.HasPrefix(hostPort, "127.0.0.1:") {
			t.Fatalf("host port = %q %v", hostPort, err)
		}
		var logs ContainerLogs
		deadline := time.Now().Add(5 * time.Second)
		for {
			logs, err = scope.Logs(ctx, "client", 1024)
			if err == nil && strings.Contains(string(logs.Stdout), "topology-ready") && strings.Contains(string(logs.Stderr), "topology-diagnostic") && !logs.Truncated {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("logs = %#v %v", logs, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		sample, err := scope.Sample(ctx, "client")
		if err != nil || sample.Role != "client" || sample.PIDs == 0 {
			t.Fatalf("sample = %#v %v", sample, err)
		}
	}
	if first.RunID() == second.RunID() {
		t.Fatal("concurrent scopes share a run ID")
	}
	if err := first.Disconnect(ctx, "client", "control"); err != nil {
		t.Fatal(err)
	}
	if err := first.Reconnect(ctx, "client", "control"); err != nil {
		t.Fatal(err)
	}
	if err := first.Restart(ctx, "client", time.Second); err != nil {
		t.Fatal(err)
	}
	firstReport, err := first.Close(true)
	if err != nil {
		t.Fatal(err)
	}
	firstClosed = true
	if firstReport.RemovedContainers != 1 || firstReport.RemovedNetworks != 2 || firstReport.RemovedVolumes != 1 {
		t.Fatalf("first cleanup = %#v", firstReport)
	}
	if err := second.Restart(ctx, "client", time.Second); err != nil {
		t.Fatalf("first cleanup touched second run: %v", err)
	}
	resource := second.containers["client"]
	if code, err := resource.Exec([]string{"sh", "-c", "test \"$(cat /state/marker)\" = \"$PAPERBOAT_MARKER\""}, dockertest.ExecOptions{}); err != nil || code != 0 {
		t.Fatalf("first cleanup touched second volume: exit=%d %v", code, err)
	}
	if _, err := second.HostPort("client", "8080/tcp"); err != nil {
		t.Fatalf("first cleanup touched second port binding: %v", err)
	}
	secondReport, err := second.Close(true)
	if err != nil {
		t.Fatal(err)
	}
	secondClosed = true
	if secondReport.RemovedContainers != 1 || secondReport.RemovedNetworks != 2 || secondReport.RemovedVolumes != 1 {
		t.Fatalf("second cleanup = %#v", secondReport)
	}
	for _, runID := range []string{first.RunID(), second.RunID()} {
		containers, err := second.pool.Client.ListContainers(dockerListByRun(runID))
		if err != nil || len(containers) != 0 {
			t.Fatalf("containers retained for %s: %d %v", runID, len(containers), err)
		}
		for _, network := range mustNetworks(t, second) {
			if network.Labels[RunLabel] == runID {
				t.Fatalf("network %s retained for %s", network.Name, runID)
			}
		}
	}
	if len(first.Evidence().Events) < 8 || len(second.Evidence().Events) < 6 {
		t.Fatal("bounded lifecycle evidence is incomplete")
	}
}

func TestNetworkFaultInjectionAffectsOwnedUDPAndRecovers(t *testing.T) {
	if os.Getenv("PAPERBOAT_TOPOLOGY_TEST") != "1" {
		t.Skip("set PAPERBOAT_TOPOLOGY_TEST=1 to run Docker fault injection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	scope := newIntegrationScope(t, ctx, "pbfault")
	closed := false
	t.Cleanup(func() {
		if !closed {
			_, _ = scope.Close(true)
		}
	})
	if err := scope.CreateNetwork(ctx, "peer", true); err != nil {
		t.Fatal(err)
	}
	if err := scope.CreateVolume(ctx, "state"); err != nil {
		t.Fatal(err)
	}
	if err := scope.RunContainer(ctx, ContainerSpec{
		Role: "server", Image: topologyNetToolsImage,
		Command:  []string{"sh", "-c", "touch /state/messages; exec socat -u UDP4-RECVFROM:9000,fork SYSTEM:'cat >> /state/messages'"},
		Networks: []string{"peer"}, Volumes: []VolumeMount{{Role: "state", ContainerPath: "/state"}},
		MemoryBytes: 64 << 20, CPUQuota: 25000, PIDs: 64, NetworkAdmin: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.RunContainer(ctx, ContainerSpec{
		Role: "client", Image: topologyNetToolsImage,
		Command:  []string{"sh", "-c", "while :; do sleep 60; done"},
		Networks: []string{"peer"}, MemoryBytes: 64 << 20, CPUQuota: 25000, PIDs: 64, NetworkAdmin: true,
	}); err != nil {
		t.Fatal(err)
	}
	serverAddress, err := scope.ContainerAddress("server", "peer")
	if err != nil {
		t.Fatal(err)
	}
	send := func(token string) error {
		t.Helper()
		return scope.Exec(ctx, "client", []string{"sh", "-c", "printf %s " + token + " | socat - UDP4-DATAGRAM:" + serverAddress + ":9000"})
	}
	requireSend := func(token string) {
		t.Helper()
		if err := send(token); err != nil {
			t.Fatal(err)
		}
	}
	waitForReceipt := func(token string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if err := scope.Exec(ctx, "server", []string{"grep", "-Fq", token, "/state/messages"}); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("server did not receive %q", token)
	}
	requireNoReceipt := func(token string) {
		t.Helper()
		if err := scope.Exec(ctx, "server", []string{"sh", "-c", "! grep -Fq " + token + " /state/messages"}); err != nil {
			t.Fatalf("server unexpectedly received %q: %v", token, err)
		}
	}
	requireSend("baseline")
	waitForReceipt("baseline")
	if err := scope.SetNetworkImpairment("client", 0, 100); err != nil {
		t.Fatal(err)
	}
	requireSend("dropped")
	time.Sleep(300 * time.Millisecond)
	requireNoReceipt("dropped")
	if err := scope.SetNetworkImpairment("client", 0, 0); err != nil {
		t.Fatal(err)
	}
	requireSend("recovered")
	waitForReceipt("recovered")
	if err := scope.SetUDPBlocked("client", true); err != nil {
		t.Fatal(err)
	}
	if err := send("blocked"); err != nil && !errors.Is(err, ErrCommand) {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	requireNoReceipt("blocked")
	if err := scope.SetUDPBlocked("client", false); err != nil {
		t.Fatal(err)
	}
	requireSend("unblocked")
	waitForReceipt("unblocked")
	report, err := scope.Close(true)
	if err != nil {
		t.Fatal(err)
	}
	closed = true
	if report.RemovedContainers != 2 || report.RemovedNetworks != 1 || report.RemovedVolumes != 1 {
		t.Fatalf("cleanup=%+v", report)
	}
}

func newIntegrationScope(t *testing.T, ctx context.Context, prefix string) *Scope {
	t.Helper()
	scope, err := New(ctx, Config{NamePrefix: prefix, MaxWait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func dockerListByRun(runID string) docker.ListContainersOptions {
	return docker.ListContainersOptions{All: true, Filters: map[string][]string{"label": {RunLabel + "=" + runID}}}
}

func mustNetworks(t *testing.T, scope *Scope) []docker.Network {
	t.Helper()
	networks, err := scope.pool.Client.ListNetworks()
	if err != nil {
		t.Fatal(err)
	}
	return networks
}

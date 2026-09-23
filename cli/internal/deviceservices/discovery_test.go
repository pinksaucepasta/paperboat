package deviceservices

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	services, err := Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, s := range services {
		if s.Port == 0 {
			t.Errorf("expected non-zero port")
		}
		if s.Loopback != "127.0.0.1" && s.Loopback != "::1" {
			t.Errorf("unexpected loopback %q", s.Loopback)
		}
	}
}

func TestSnapshotIncludesOwnedListenerAndExcludesNoncanonicalLoopback(t *testing.T) {
	owned := listenTCP4(t, "127.0.0.1:0")
	defer owned.Close()
	ownedPort := uint16(owned.Addr().(*net.TCPAddr).Port)

	noncanonical, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Logf("noncanonical loopback unavailable on this OS: %v", err)
	}
	var noncanonicalPort uint16
	if err == nil {
		defer noncanonical.Close()
		noncanonicalPort = uint16(noncanonical.Addr().(*net.TCPAddr).Port)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		services, snapshotErr := Snapshot(context.Background())
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		ownedFound, noncanonicalFound := false, false
		for _, service := range services {
			ownedFound = ownedFound || service.Port == ownedPort && service.Loopback == "127.0.0.1"
			noncanonicalFound = noncanonicalFound || noncanonicalPort != 0 && service.Port == noncanonicalPort
		}
		if noncanonicalFound {
			t.Fatalf("noncanonical 127.0.0.2 listener %d was advertised as canonical loopback", noncanonicalPort)
		}
		if ownedFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned listener %d absent from snapshot: %#v", ownedPort, services)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func listenTCP4(t *testing.T, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

func TestNormalize(t *testing.T) {
	input := []Service{
		{Port: 8080, Loopback: "127.0.0.1"},
		{Port: 8080, Loopback: "127.0.0.1"}, // Duplicate
		{Port: 3000, Loopback: "::1"},
		{Port: 0, Loopback: "127.0.0.1"},      // Invalid port
		{Port: 9000, Loopback: "192.168.1.1"}, // Non-loopback
	}

	output := normalize(input)
	if len(output) != 2 {
		t.Fatalf("expected 2 services, got %d", len(output))
	}

	if output[0].Port != 3000 || output[0].Loopback != "::1" {
		t.Errorf("unexpected first service: %+v", output[0])
	}
	if output[1].Port != 8080 || output[1].Loopback != "127.0.0.1" {
		t.Errorf("unexpected second service: %+v", output[1])
	}
}

// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/tstest/integration"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

func startUDPLifecyclePair(t *testing.T, idle time.Duration, handler func(ConnPacketConn)) (*Server, *Server) {
	t.Helper()
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	region := dm.Regions[1]
	if region == nil {
		t.Fatal("no region 1 in DERP map")
	}

	clientKey, serverKey := key.NewNode(), key.NewNode()
	clientAddr := netip.MustParseAddr("fd7a:115c:a1e0::51")
	serverAddr := netip.MustParseAddr("fd7a:115c:a1e0::52")
	server := &Server{
		Key:            serverKey,
		LocalAddr:      serverAddr,
		AllowedPeers:   map[key.NodePublic]netip.Addr{clientKey.Public(): clientAddr},
		Region:         region,
		ServedUDPPorts: []filter.PortRange{{First: 53, Last: 53}},
		UDPIdleTimeout: idle,
		OnUDP: func(port uint16) func(ConnPacketConn) {
			if port != 53 {
				return nil
			}
			return handler
		},
		Logf: logger.Discard,
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start UDP server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	client := &Server{
		Key:            clientKey,
		LocalAddr:      clientAddr,
		AllowedPeers:   map[key.NodePublic]netip.Addr{serverKey.Public(): serverAddr},
		Region:         region,
		ServedUDPPorts: []filter.PortRange{},
		Logf:           logger.Discard,
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start UDP client engine: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, server
}

func dialUDPLifecycleFlow(t *testing.T, client, server *Server) ConnPacketConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	flow, err := client.DialAuthorizedUDP(ctx, server.Key.Public(), server.lb.discoPublic(), netip.AddrPortFrom(server.LocalAddr, 53))
	if err != nil {
		t.Fatalf("dial authorized UDP: %v", err)
	}
	t.Cleanup(func() { _ = flow.Close() })
	return flow
}

func TestUDPIdleTimeout(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan error, 1)
	client, server := startUDPLifecyclePair(t, 500*time.Millisecond, func(connection ConnPacketConn) {
		defer connection.Close()
		close(handlerStarted)
		buffer := make([]byte, 1)
		if _, err := connection.Read(buffer); err != nil {
			handlerDone <- err
			return
		}
		_, err := connection.Read(buffer)
		handlerDone <- err
	})
	flow := dialUDPLifecycleFlow(t, client, server)
	if _, err := flow.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("UDP handler did not start")
	}
	select {
	case err := <-handlerDone:
		if err == nil {
			t.Fatal("UDP flow ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UDP flow did not time out")
	}
}

func TestUDPIdleTimeoutDefault(t *testing.T) {
	if got := (&Server{}).udpIdleTimeout(); got != DefaultUDPIdleTimeout {
		t.Fatalf("zero UDPIdleTimeout = %v; want DefaultUDPIdleTimeout %v", got, DefaultUDPIdleTimeout)
	}
	const custom = 5 * time.Second
	if got := (&Server{UDPIdleTimeout: custom}).udpIdleTimeout(); got != custom {
		t.Fatalf("custom UDPIdleTimeout = %v; want %v", got, custom)
	}
}

func TestUDPIdleTimeoutReset(t *testing.T) {
	const idle = 750 * time.Millisecond
	handlerDone := make(chan error, 1)
	client, server := startUDPLifecyclePair(t, idle, func(connection ConnPacketConn) {
		defer connection.Close()
		buffer := make([]byte, 32)
		for {
			n, err := connection.Read(buffer)
			if err != nil {
				handlerDone <- err
				return
			}
			if _, err := connection.Write(buffer[:n]); err != nil {
				handlerDone <- err
				return
			}
		}
	})
	flow := dialUDPLifecycleFlow(t, client, server)
	if err := flow.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Exchanges continue past the original idle deadline. Each successful read
	// and write must retain the flow until activity actually stops.
	for i := range 5 {
		message := []byte{byte('a' + i)}
		if _, err := flow.Write(message); err != nil {
			t.Fatalf("UDP write %d: %v", i, err)
		}
		buffer := make([]byte, 32)
		n, err := flow.Read(buffer)
		if err != nil {
			t.Fatalf("UDP read %d (flow closed despite activity): %v", i, err)
		}
		if !bytes.Equal(buffer[:n], message) {
			t.Fatalf("UDP echo %d = %q; want %q", i, buffer[:n], message)
		}
		time.Sleep(200 * time.Millisecond)
	}
	select {
	case err := <-handlerDone:
		if err == nil {
			t.Fatal("UDP flow ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UDP flow did not time out after going idle")
	}
}

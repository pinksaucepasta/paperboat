package tunnelmanager

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
)

type originDialer struct {
	mu      sync.Mutex
	network string
	address string
	err     error
}

func (d *originDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.network, d.address = network, address
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	go server.Close()
	return client, nil
}

type originTLSProviderFunc func(context.Context, hoststate.TunnelConfigRoute) (*tls.Config, error)

func (f originTLSProviderFunc) TLSConfig(ctx context.Context, route hoststate.TunnelConfigRoute) (*tls.Config, error) {
	return f(ctx, route)
}

func TestNetworkOriginProberUsesRouteNetworkAndTimeout(t *testing.T) {
	for _, test := range []struct {
		scheme  string
		address string
		network string
	}{
		{scheme: "http", address: "127.0.0.1:3000", network: "tcp"},
		{scheme: "h2c", address: "127.0.0.1:3001", network: "tcp"},
		{scheme: "tcp", address: "127.0.0.1:5432", network: "tcp"},
		{scheme: "unix", address: "/var/run/demo.sock", network: "unix"},
	} {
		dialer := &originDialer{}
		prober := NetworkOriginProber{Dialer: dialer}
		route := hoststate.TunnelConfigRoute{OriginScheme: test.scheme, OriginAddress: test.address, TLSVerification: "not_applicable", ConnectTimeoutMs: 100, DesiredState: "active"}
		if err := prober.ProbeOrigin(context.Background(), route); err != nil {
			t.Fatalf("%s probe: %v", test.scheme, err)
		}
		dialer.mu.Lock()
		if dialer.network != test.network || dialer.address != test.address {
			t.Fatalf("%s dial = %s %s", test.scheme, dialer.network, dialer.address)
		}
		dialer.mu.Unlock()
	}
}

func TestNetworkOriginProberClassifiesDialFailure(t *testing.T) {
	failed := errors.New("dial refused")
	prober := NetworkOriginProber{Dialer: &originDialer{err: failed}}
	err := prober.ProbeOrigin(context.Background(), hoststate.TunnelConfigRoute{OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", ConnectTimeoutMs: 100, DesiredState: "active"})
	if !errors.Is(err, ErrOriginUnavailable) || !errors.Is(err, failed) {
		t.Fatalf("failure = %v", err)
	}
}

func TestNetworkOriginProberRejectsLateDialAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prober := NetworkOriginProber{Dialer: &originDialer{}}
	err := prober.ProbeOrigin(ctx, hoststate.TunnelConfigRoute{OriginScheme: "http", OriginAddress: "127.0.0.1:3000", TLSVerification: "not_applicable", ConnectTimeoutMs: 100, DesiredState: "active"})
	if !errors.Is(err, ErrOriginUnavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("late canceled dial = %v", err)
	}
}

func TestNetworkOriginTLSConfigEnforcesVerificationPolicy(t *testing.T) {
	serverName := "origin.example.test"
	route := hoststate.TunnelConfigRoute{OriginScheme: "https", OriginAddress: "127.0.0.1:443", TLSVerification: "system", TLSServerName: &serverName}
	config, err := (NetworkOriginProber{}).originTLSConfig(context.Background(), route)
	if err != nil || config.ServerName != serverName || config.InsecureSkipVerify || config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("system TLS config = %#v, %v", config, err)
	}
	route.TLSVerification = "insecure"
	config, err = (NetworkOriginProber{}).originTLSConfig(context.Background(), route)
	if err != nil || !config.InsecureSkipVerify || config.VerifyConnection != nil {
		t.Fatalf("insecure TLS config = %#v, %v", config, err)
	}
	route.TLSVerification = "pinned"
	if _, err := (NetworkOriginProber{}).originTLSConfig(context.Background(), route); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("pinned without a pin = %v", err)
	}
	route.TLSVerification = "custom_ca"
	if _, err := (NetworkOriginProber{}).originTLSConfig(context.Background(), route); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("custom CA without provider = %v", err)
	}
	provider := originTLSProviderFunc(func(context.Context, hoststate.TunnelConfigRoute) (*tls.Config, error) {
		return &tls.Config{InsecureSkipVerify: true}, nil //nolint:gosec
	})
	if _, err := (NetworkOriginProber{TLS: provider}).originTLSConfig(context.Background(), route); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("provider disabled verification: %v", err)
	}
	route.TLSVerification = "mutual_tls"
	if err := (NetworkOriginProber{Dialer: &originDialer{}}).ProbeOrigin(context.Background(), route); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("obsolete mutual_tls mode = %v", err)
	}
}

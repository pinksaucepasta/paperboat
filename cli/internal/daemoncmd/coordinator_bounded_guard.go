package daemoncmd

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// boundedGuard gives every guard call its own deadline: a reconcile has none, so nothing else ends a call the guard
// never answers.
type boundedGuard struct {
	guardedNameClient
	timeout time.Duration
}

type certificateIssuer interface {
	Certificate(context.Context, string) (machineguard.CertificateBundle, error)
}

type boundedCertificateGuard struct {
	boundedGuard
	issuer certificateIssuer
}

func boundGuard(client guardedNameClient, timeout time.Duration) guardedNameClient {
	bounded := boundedGuard{client, timeout}
	if issuer, ok := client.(certificateIssuer); ok {
		return boundedCertificateGuard{bounded, issuer}
	}
	return bounded
}

func (g boundedGuard) Acquire(ctx context.Context, host string, ip netip.Addr, port int) (net.Listener, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.Acquire(ctx, host, ip, port)
}

func (g boundedGuard) AcquireMachine(ctx context.Context, account, machine, hostname string, preferred netip.Addr, port int) (net.Listener, netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.AcquireMachine(ctx, account, machine, hostname, preferred, port)
}

func (g boundedGuard) ApplyPolicy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.ApplyPolicy(ctx)
}

func (g boundedGuard) ReplaceClaims(ctx context.Context, account string, machines []string) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.ReplaceClaims(ctx, account, machines)
}

func (g boundedGuard) ReplaceNames(ctx context.Context, names map[string]netip.Addr) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.ReplaceNames(ctx, names)
}

func (g boundedGuard) ReplaceAliases(ctx context.Context, aliases map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.ReplaceAliases(ctx, aliases)
}

func (g boundedGuard) Status(ctx context.Context) (machineguard.RuntimeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.guardedNameClient.Status(ctx)
}

func (g boundedCertificateGuard) Certificate(ctx context.Context, name string) (machineguard.CertificateBundle, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	return g.issuer.Certificate(ctx, name)
}

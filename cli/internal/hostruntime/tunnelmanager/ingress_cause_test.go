package tunnelmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hoststate"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type ingressErrorSpy struct{}

func (*ingressErrorSpy) Error() string { panic("private ingress error formatted") }

type ingressCloseCause struct {
	io.ReadWriteCloser
	cause error
}

func (s ingressCloseCause) Close() error { return errors.Join(s.ReadWriteCloser.Close(), s.cause) }

type ingressFailedDialer struct{ cause error }

func (d ingressFailedDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, d.cause
}

func TestIngressLookupPreservesOperationalCauseAndFreshAdmission(t *testing.T) {
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	ctx := supportref.WithContext(t.Context(), reference)
	var faults []errorreport.Fault
	restoreFaults := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restoreFaults()
	decision, open := browserDecisionFixture(time.Now().UTC())
	route := hoststate.TunnelConfigRoute{ID: decision.Binding.RouteID, Protocol: "http", OriginScheme: "http", OriginAddress: decision.Binding.OriginAddress, TLSVerification: "not_applicable"}
	spy := &ingressErrorSpy{}
	cause := errors.Join(syscall.EIO, spy)
	var fail atomic.Bool
	fail.Store(true)
	f := OriginStreamForwarder{IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		if fail.Load() {
			return connectorprotocol.IngressDecision{}, cause
		}
		return decision, nil
	}}
	admit := func() (context.Context, context.CancelFunc, error) {
		input, output := net.Pipe()
		defer output.Close()
		written := make(chan error, 1)
		go func() {
			written <- connectorprotocol.WriteIngressDecision(input, decision, time.Now().UTC())
			input.Close()
		}()
		admitted, release, err := f.admitIngress(ctx, output, open, route)
		if writeErr := <-written; writeErr != nil {
			t.Fatal("decision write failed")
		}
		return admitted, release, err
	}
	_, _, err := admit()
	if !errors.Is(err, syscall.EIO) || !errors.Is(err, spy) || errors.Is(err, connectorprotocol.ErrIngressDenied) {
		t.Fatal("lookup cause became denial")
	}
	if err.Error() != "Tunnel ingress operation failed." {
		t.Fatal("unsafe operational presentation")
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatal("correlated original local fault missing")
	}

	fail.Store(false)
	admitted, release, err := admit()
	if err != nil || admitted == nil || release == nil {
		t.Fatal("fresh authority did not recover")
	}
	release()
	if len(faults) != 1 {
		t.Fatal("healthy recovery duplicated fault")
	}
}

func TestIngressRefreshPreservesLookupAndCloseCausesAndRecovers(t *testing.T) {
	decision, open := browserDecisionFixture(time.Now().UTC())
	cause := errors.Join(connectorprotocol.ErrIngressDenied, syscall.EIO, &ingressErrorSpy{})
	f := OriginStreamForwarder{IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return connectorprotocol.IngressDecision{}, cause
	}}
	ctx, cancel := context.WithCancelCause(t.Context())
	input, output := net.Pipe()
	defer input.Close()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	_, err := f.refreshIngress(ctx, open, decision, decision)
	if !errors.Is(err, syscall.EIO) {
		t.Fatal("refresh lost operational cause")
	}
	terminate := ingressTermination(ctx, cancel, ingressCloseCause{ReadWriteCloser: output, cause: syscall.ENOSPC})
	result := terminate(err)
	if !errors.Is(context.Cause(ctx), syscall.EIO) || !errors.Is(result, syscall.ENOSPC) {
		t.Fatal("termination lost refresh/close cause")
	}
	if len(faults) != 1 {
		t.Fatal("mixed denial unexpectedly quiet or duplicated")
	}
	if _, err := input.Write([]byte{1}); err == nil {
		t.Fatal("revoked stream stayed open")
	}
	f.IngressAuthority = func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return decision, nil
	}
	if _, err := f.refreshIngress(t.Context(), open, decision, decision); err != nil {
		t.Fatal("fresh refresh did not recover")
	}
	if len(faults) != 1 {
		t.Fatal("healthy refresh emitted failure")
	}
}

type ingressCycle struct{}

func (*ingressCycle) Error() string   { panic("cycle formatted") }
func (c *ingressCycle) Unwrap() error { return c }
func TestIngressDenialRequiresCompleteCauseProof(t *testing.T) {
	shared := fmt.Errorf("private: %w", connectorprotocol.ErrIngressDenied)
	if !ingressDenialOnly(errors.Join(shared, shared)) {
		t.Fatal("pure repeated denial lost")
	}
	var typedNil *ingressCycle
	for _, err := range []error{errors.Join(connectorprotocol.ErrIngressDenied, syscall.EIO), errors.Join(connectorprotocol.ErrIngressDenied, &ingressCycle{}), errors.Join(connectorprotocol.ErrIngressDenied, typedNil)} {
		if ingressDenialOnly(err) {
			t.Fatal("independent or unresolved cause became denial")
		}
	}
}

func TestIngressTCPDialPreservesOriginalCause(t *testing.T) {
	cause := errors.Join(syscall.ECONNREFUSED, &ingressErrorSpy{})
	f := OriginStreamForwarder{Transport: &OriginHTTPTransport{Dialer: ingressFailedDialer{cause: cause}}, IngressAuthority: func(context.Context, connectorprotocol.StreamOpen, connectorprotocol.IngressDecision) (connectorprotocol.IngressDecision, error) {
		return connectorprotocol.IngressDecision{}, nil
	}}
	input, output := net.Pipe()
	defer input.Close()
	defer output.Close()
	err := f.serveTCP(t.Context(), output, hoststate.TunnelConfigRoute{Protocol: "tcp", OriginScheme: "tcp", OriginAddress: "127.0.0.1:1"})
	if !errors.Is(err, ErrOriginUnavailable) || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatal("TCP dial lost cause")
	}
	if err.Error() != "Tunnel ingress operation failed." {
		t.Fatal("TCP dial exposed cause text")
	}
}

func TestIngressExpiryDoesNotHideLateAuthorityFailure(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	input, output := net.Pipe()
	defer input.Close()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	terminate := ingressTermination(ctx, cancel, output)
	if err := terminate(nil); err != nil {
		t.Fatal("normal expiry close failed")
	}
	if err := terminate(syscall.EIO); !errors.Is(err, syscall.EIO) {
		t.Fatal("late authority error lost")
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) {
		t.Fatal("expiry hid late operational failure")
	}
}

func TestIngressTerminationQuietClosePreservesLateOperationalOwner(t *testing.T) {
	for _, fixture := range []struct {
		name string
		open func(*testing.T) io.Closer
	}{
		{"cached_data_carrier", closedIngressCarrier},
		{"already_closed_tcp", closedIngressTCP},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stream := fixture.open(t)
			ctx, cancel := context.WithCancelCause(t.Context())
			var faults []errorreport.Fault
			restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
			defer restore()
			terminate := ingressTermination(ctx, cancel, stream)
			closeErr := terminate(nil)
			if closeErr != nil && !ingressExpectedOnly(closeErr, true) {
				t.Fatal("healthy teardown did not preserve expected close")
			}
			if len(faults) != 0 {
				t.Fatal("normal closed-stream teardown captured failure")
			}
			original := errors.Join(syscall.EIO, &ingressErrorSpy{})
			err := terminate(original)
			if !errors.Is(err, syscall.EIO) {
				t.Fatal("late operational cause was lost")
			}
			if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].Outcome != "failed" {
				t.Fatal("quiet close consumed final operational capture")
			}
		})
	}
}

func closedIngressTCP(t *testing.T) io.Closer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("listen failed")
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	client, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal("dial failed")
	}
	defer client.Close()
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("accept did not finish")
	}
	if err := server.Close(); err != nil {
		t.Fatal("initial connection close failed")
	}
	return server
}

func closedIngressCarrier(t *testing.T) io.Closer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	left, right := net.Pipe()
	identity := connector.DataCarrierIdentity{AccountID: "account-1", HostID: "host-1", TunnelID: "tunnel-1", ConnectorID: "connector-1", SessionID: "session-1", ProcessGeneration: 1, Generation: 1}
	config := connector.DefaultDataCarrierConfig()
	server, err := connector.NewDataCarrierServer(ctx, right, config, connector.DataCarrierAdmission{Identity: identity, Authorize: func(context.Context, connector.StreamOpen) error { return nil }})
	if err != nil {
		left.Close()
		right.Close()
		t.Fatal("server carrier failed")
	}
	t.Cleanup(func() { server.Close() })
	client, err := connector.NewDataCarrierClient(ctx, left, config, identity)
	if err != nil {
		left.Close()
		t.Fatal("client carrier failed")
	}
	t.Cleanup(func() { client.Close() })
	_, open := browserDecisionFixture(time.Now().UTC())
	stream, err := client.OpenStream(ctx, open)
	if err != nil {
		t.Fatal("carrier stream open failed")
	}
	remote, _, err := server.AcceptStream(ctx)
	if err != nil {
		stream.Close()
		t.Fatal("carrier accept failed")
	}
	t.Cleanup(func() { remote.Close() })
	first := stream.Close()
	second := stream.Close()
	if first != nil || second != nil {
		t.Fatal("healthy carrier cached close failed")
	}
	return stream
}

func TestIngressTerminationExpectedLeavesAreLifecycleScoped(t *testing.T) {
	for _, err := range []error{net.ErrClosed, os.ErrClosed, context.Canceled, errors.Join(net.ErrClosed, connectorprotocol.ErrIngressDenied)} {
		if !ingressExpectedOnly(err, true) {
			t.Fatal("pure expected lifecycle cause rejected")
		}
		if ingressDenialOnly(err) {
			t.Fatal("lifecycle-only closed error hid initial failure")
		}
	}
	var typedNil *ingressCycle
	for _, err := range []error{errors.Join(net.ErrClosed, syscall.EIO), errors.Join(os.ErrClosed, &ingressCycle{}), errors.Join(context.Canceled, typedNil)} {
		if ingressExpectedOnly(err, true) {
			t.Fatal("mixed/unresolved lifecycle failure became quiet")
		}
	}
}

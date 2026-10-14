package machineservices

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type explicitCloseFailureConn struct {
	net.Conn
	err        error
	closeCalls *atomic.Int32
}

func (c explicitCloseFailureConn) Close() error {
	_ = c.Conn.Close()
	if c.closeCalls != nil {
		c.closeCalls.Add(1)
	}
	return c.err
}

func TestExplicitProbeExactTargetAndConnectionCleanup(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var data [1]byte
		_, err = conn.Read(data[:])
		closed <- err
	}()
	service := Service{Port: port, Loopback: "127.0.0.1"}
	observed, err := ProbeExplicit(t.Context(), []Service{service, service})
	if err != nil || !reflect.DeepEqual(observed, []Service{service}) {
		t.Fatalf("explicit target: %+v %v", observed, err)
	}
	if err := <-closed; !errors.Is(err, io.EOF) {
		t.Fatalf("probe leaked connection or sent payload: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := ProbeExplicit(t.Context(), []Service{service}); err != nil || len(got) != 0 {
		t.Fatalf("closed target announced: %v %v", got, err)
	}
}

func TestExplicitProbeBoundsAndCancellation(t *testing.T) {
	for _, services := range [][]Service{{{Port: 1, Loopback: "127.0.0.2"}}, {{Port: 0, Loopback: "127.0.0.1"}}, make([]Service, MaximumExplicitServices+1)} {
		if _, err := ProbeExplicit(t.Context(), services); !errors.Is(err, ErrExplicitServicesInvalid) {
			t.Fatal("invalid targets accepted", err)
		}
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	entered := make(chan struct{}, 8)
	var active, maximum atomic.Int32
	services := make([]Service, 8)
	for i := range services {
		services[i] = Service{Port: uint16(i + 1), Loopback: "127.0.0.1"}
	}
	done := make(chan error, 1)
	go func() {
		_, err := probeExplicit(ctx, services, func(probeCtx context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" {
				t.Error("wrong protocol")
			}
			deadline, ok := probeCtx.Deadline()
			if !ok || time.Until(deadline) > explicitProbeTimeout {
				t.Error("missing bounded deadline")
			}
			count := active.Add(1)
			for old := maximum.Load(); count > old; old = maximum.Load() {
				if maximum.CompareAndSwap(old, count) {
					break
				}
			}
			entered <- struct{}{}
			<-probeCtx.Done()
			active.Add(-1)
			return nil, errors.Join(probeCtx.Err(), syscall.ENOSPC)
		})
		done <- err
	}()
	for range explicitProbeConcurrency {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("probe workers did not start")
		}
	}
	cancel(syscall.EIO)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.EIO) || !errors.Is(err, syscall.ENOSPC) {
			t.Fatal("mixed cancellation and operational causes were lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not stop")
	}
	if maximum.Load() != explicitProbeConcurrency || active.Load() != 0 {
		t.Fatal("unbounded or leaked workers", maximum.Load(), active.Load())
	}

	service := Service{Port: 54321, Loopback: "127.0.0.1"}
	observed, err := probeExplicit(t.Context(), []Service{service}, func(context.Context, string, string) (net.Conn, error) {
		return nil, syscall.EIO
	})
	if len(observed) != 0 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("unexpected native dial failure was discarded: %+v %v", observed, err)
	}
	observed, err = probeExplicit(t.Context(), []Service{service}, func(context.Context, string, string) (net.Conn, error) {
		return nil, context.DeadlineExceeded
	})
	if err != nil || len(observed) != 0 {
		t.Fatalf("per-target timeout was not ordinary absence: %+v %v", observed, err)
	}
	observed, err = probeExplicit(t.Context(), []Service{service}, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.Join(syscall.ECONNREFUSED, syscall.EIO)
	})
	if len(observed) != 0 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("mixed refusal and operational failure was suppressed: %+v %v", observed, err)
	}
	client, server := net.Pipe()
	defer server.Close()
	observed, err = probeExplicit(t.Context(), []Service{service}, func(context.Context, string, string) (net.Conn, error) {
		return explicitCloseFailureConn{Conn: client, err: syscall.EIO}, nil
	})
	if len(observed) != 0 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("connection Close failure was discarded: %+v %v", observed, err)
	}
	partialClient, partialServer := net.Pipe()
	defer partialServer.Close()
	t.Cleanup(func() { _ = partialClient.Close() })
	var partialCloseCalls atomic.Int32
	partialCloseFailure := explicitCloseFailureConn{Conn: partialClient, err: syscall.EIO, closeCalls: &partialCloseCalls}
	dialFailure := syscall.ENOSPC
	observed, err = probeExplicit(t.Context(), []Service{service}, func(context.Context, string, string) (net.Conn, error) {
		return partialCloseFailure, dialFailure
	})
	if len(observed) != 0 || !errors.Is(err, dialFailure) || !errors.Is(err, syscall.EIO) || partialCloseCalls.Load() != 1 {
		t.Fatalf("partial connection or either owned failure was lost: %+v %v close_calls=%d", observed, err, partialCloseCalls.Load())
	}
}

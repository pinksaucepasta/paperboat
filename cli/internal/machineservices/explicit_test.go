package machineservices

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
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
			return nil, probeCtx.Err()
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
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not stop")
	}
	if maximum.Load() != explicitProbeConcurrency || active.Load() != 0 {
		t.Fatal("unbounded or leaked workers", maximum.Load(), active.Load())
	}
}

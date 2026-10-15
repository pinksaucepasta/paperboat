package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
)

type nativeBridgeWriteFailure struct {
	net.Conn
	cause   error
	entered chan struct{}
}

func (c *nativeBridgeWriteFailure) Write([]byte) (int, error) { close(c.entered); return 0, c.cause }
func (c *nativeBridgeWriteFailure) CloseWrite() error         { return c.Conn.(*net.TCPConn).CloseWrite() }

func TestNativeDispatchBridgeFailureReleasesMaintenanceAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "worker", Version: "1", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	ready := hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}
	if err = controller.MarkReady(ready); err != nil {
		t.Fatal(err)
	}
	if _, err = controller.Activate(hostdproto.Activate{WorkerID: ready.WorkerID, APIVersion: ready.APIVersion, Epoch: ready.Epoch, Lease: ready.Lease}); err != nil {
		t.Fatal(err)
	}

	peer, left := nativeBridgeTCPPair(t)
	origin, right := nativeBridgeTCPPair(t)
	admissionCtx := context.WithValue(ctx, nativeHTTPAdmissionKey{}, nativeHTTPAdmission(func(c context.Context) (func(), <-chan struct{}, error) {
		release, err := controller.AcquireActive(welcome.WorkerID, welcome.Epoch)
		return release, c.Done(), err
	}))
	served := make(chan error, 1)
	go func() {
		served <- ServeHTTPConnection(admissionCtx, origin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Exceed socket buffering so a stranded bridge retains the real request
			// admission rather than allowing a tiny response to finish independently.
			chunk := make([]byte, 1<<20)
			for {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
	}()
	defer func() {
		cancel()
		origin.Close()
		select {
		case <-served:
		case <-time.After(time.Second):
			t.Error("HTTP owner cleanup did not finish")
		}
	}()

	done := make(chan error, 1)
	finished := false
	defer func() {
		peer.Close()
		left.Close()
		origin.Close()
		right.Close()
		if !finished {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("bridge cleanup did not finish")
			}
		}
	}()
	cause := errors.New("native stream receive side reset")
	entered := make(chan struct{})
	go func() {
		done <- BridgeNativeDispatch(&nativeBridgeWriteFailure{Conn: left, cause: cause, entered: entered}, right)
	}()
	if _, err := fmt.Fprint(peer, "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("native write fault was not reached")
	}
	select {
	case err := <-done:
		finished = true
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok || len(joined.Unwrap()) == 0 || !errors.Is(joined.Unwrap()[0], cause) {
			t.Fatalf("bridge discarded primary stream failure: %v", err)
		}
	case <-time.After(time.Second):
		// A clean TCP half-close must remain supported, but a failed write cannot
		// leave the opposite read (and its native operation) waiting indefinitely.
		blocked, stop := context.WithTimeout(ctx, 100*time.Millisecond)
		defer stop()
		_, maintenanceErr := controller.PrepareMaintenance(blocked, false, nil)
		if !errors.Is(maintenanceErr, hostdproto.ErrMaintenanceBusy) {
			t.Fatalf("open bridge maintenance state=%v", maintenanceErr)
		}
		t.Fatal("failed native stream retained maintenance admission and the opposite IPC read")
	}
	maintenanceCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err = controller.PrepareMaintenance(maintenanceCtx, false, nil); err != nil {
		t.Fatalf("failed stream retained native maintenance admission: %v", err)
	}
	controller.AbortMaintenance()
}

func nativeBridgeTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.AcceptTCP()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	return client, server
}

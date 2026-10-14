package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestNativeDispatchAuthorizesBeforeForwardingAndPreservesHalfClose(t *testing.T) {
	// Accepted long-running work must outlive the setup credential deadline.
	for _, consumer := range []string{"terminal", "exec", "file_transfer"} {
		t.Run(consumer, func(t *testing.T) { testNativeDispatchAcceptedStream(t, consumer) })
	}
}

func testNativeDispatchAcceptedStream(t *testing.T, consumer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var opened atomic.Int32
	lost := make(chan struct{})
	released := make(chan struct{}, 4)
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(_ context.Context, worker string, epoch uint64) (func(), <-chan struct{}, error) {
			if worker != "worker" || epoch != 7 {
				return nil, nil, hostdproto.ErrFenced
			}
			return func() { released <- struct{}{} }, lost, nil
		},
		Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
			if h.Credential != "allowed" {
				return "", ErrStreamAuthorization
			}
			return "access-session", nil
		},
		ServeStream: func(_ context.Context, h streamauth.Header, conn net.Conn) error {
			opened.Add(1)
			payload, err := io.ReadAll(conn)
			if err != nil {
				return err
			}
			_, err = conn.Write(append([]byte("reply:"), payload...))
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second, Active: func(context.Context) (hostdproto.Status, error) {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 7}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	denied, _ := streamauth.New("operation", consumer, "stream", "denied", time.Now().Add(2*time.Second), 1<<20)
	if _, err := client.Open(ctx, denied); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("denied=%v", err)
	}
	if opened.Load() != 0 {
		t.Fatal("denied credential reached feature handler")
	}
	allowed := denied
	allowed.Credential = "allowed"
	connection, err := client.Open(ctx, allowed)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	// The initial native fence is released before an idle terminal association
	// can block a routing update; later terminal messages use owner fences.
	for i := 0; i < 2; i++ {
		select {
		case <-released:
		case <-ctx.Done():
			t.Fatal("terminal association retained admission")
		}
	}
	// Setup credentials bound fresh admissions, not the lifetime of an
	// accepted terminal. The terminal server owns role expiry/revocation.
	deadline := time.NewTimer(time.Until(time.Unix(allowed.DeadlineUnix, 0)) + 20*time.Millisecond)
	defer deadline.Stop()
	select {
	case <-deadline.C:
	case <-ctx.Done():
		t.Fatal("setup deadline did not elapse")
	}
	if _, err := client.Open(ctx, allowed); err == nil {
		t.Fatal("expired setup credential admitted a fresh terminal")
	}
	if _, err := connection.Write([]byte{0, 1, 2, 255}); err != nil {
		t.Fatal(err)
	}
	if err := connection.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(connection)
	if err != nil || string(reply) != string(append([]byte("reply:"), 0, 1, 2, 255)) {
		t.Fatalf("reply=%v err=%v", reply, err)
	}
	if opened.Load() != 1 {
		t.Fatalf("handler calls=%d", opened.Load())
	}
	stale, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second, Active: func(context.Context) (hostdproto.Status, error) {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "old", Epoch: 6}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stale.Open(ctx, allowed); !errors.Is(err, hostdproto.ErrFenced) {
		t.Fatalf("stale=%v", err)
	}
	wrong, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("b", 43), DialTimeout: time.Second, Active: client.config.Active})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.Open(ctx, allowed); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("capability=%v", err)
	}
}

func TestNativeDispatchClosesStreamWhenNativeAdmissionIsLost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lost := make(chan struct{})
	stopped := make(chan struct{})
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 1, HandshakeTimeout: time.Second,
		Fence: func(context.Context, string, uint64) (func(), <-chan struct{}, error) { return func() {}, lost, nil }, Authorize: func(context.Context, streamauth.Header) (string, error) { return "access", nil },
		ServeStream: func(ctx context.Context, _ streamauth.Header, _ net.Conn) error {
			<-ctx.Done()
			close(stopped)
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, _ := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second, Active: func(context.Context) (hostdproto.Status, error) {
		return hostdproto.Status{State: hostdproto.StateActive, WorkerID: "worker", Epoch: 1}, nil
	}})
	header, _ := streamauth.New("operation", "config_compare", "stream", "allowed", time.Now().Add(time.Minute), 1<<20)
	conn, err := client.Open(ctx, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	close(lost)
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("lost native owner left dispatch alive")
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("fenced stream still readable")
	}
}

//go:build linux || darwin

package server

import (
	"bufio"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/shorttemp"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

// This gate blocks the real HTTP server's final flush, after handler return.
// It exposes that independently observable lease boundary without changing the server.
type nativeHTTPFlushGateConn struct {
	net.Conn
	ctx                    context.Context
	entered, allow, closed chan struct{}
	first                  atomic.Bool
	closeOnce              sync.Once
}

func (c *nativeHTTPFlushGateConn) Write(b []byte) (int, error) {
	if c.first.CompareAndSwap(false, true) {
		close(c.entered)
		select {
		case <-c.allow:
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.ctx.Done():
			return 0, c.ctx.Err()
		}
	}
	return c.Conn.Write(b)
}
func (c *nativeHTTPFlushGateConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// Exercise the actual keepalive association against the native maintenance owner.
func TestFileTransferAdmissionEndsAtResponseAndFencesIdleAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	activate := func(id string) hostdproto.Status {
		w, e := controller.Negotiate(hostdproto.Hello{WorkerID: id, Version: "1", APIMin: 1, APIMax: 1})
		if e != nil {
			t.Fatal(e)
		}
		if e = controller.MarkReady(hostdproto.Ready{WorkerID: w.WorkerID, APIVersion: w.APIVersion, Epoch: w.Epoch, Lease: w.Lease}); e != nil {
			t.Fatal(e)
		}
		s, e := controller.Activate(hostdproto.Activate{WorkerID: w.WorkerID, APIVersion: w.APIVersion, Epoch: w.Epoch, Lease: w.Lease})
		if e != nil {
			t.Fatal(e)
		}
		return s
	}
	active := activate("worker")
	root := shorttemp.Dir(t)
	adapter, err := pty.NewShellAdapter(root)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewManager(session.ManagerConfig{Launch: func(command pty.Command) (session.PTYProcess, error) { return adapter.Start(command) }})
	if err != nil {
		t.Fatal(err)
	}
	defer sessions.Shutdown(context.Background())
	executions, err := execprocess.New(execprocess.Config{WorkspaceRoot: root, MaximumActive: 1, MaximumOperations: 2, ReplayBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer executions.Shutdown(context.Background())
	token := make([]byte, 32)
	token[0] = 7
	bridgeServer := &workloadbridge.Server{Endpoint: filepath.Join(root, "ipc", "owner.sock"), Token: token, Sessions: sessions, Executions: &execprocess.RemoteServer{Manager: executions, MaximumReaders: 1}, Fence: controller.Status, AcquireActive: controller.AcquireActive, MaximumConcurrent: 4, Ready: make(chan error, 1)}
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridgeServer.Run(ctx) }()
	if err := <-bridgeServer.Ready; err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-bridgeDone; err != nil {
			t.Error(err)
		}
	}()
	bridge, err := workloadbridge.NewClient(bridgeServer.Endpoint, token, active.WorkerID, active.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	flushEntered, flushAllow := make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	allowFlush := func() { allowOnce.Do(func() { close(flushAllow) }) }
	defer allowFlush()

	entered := make(chan struct{}, 4)
	var calls atomic.Int32
	var associations atomic.Int32
	resource := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		if _, e := io.Copy(io.Discard, r.Body); e != nil {
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(c context.Context, id string, epoch uint64) (func(), <-chan struct{}, error) {
			if id != active.WorkerID || epoch != active.Epoch {
				return nil, nil, hostdproto.ErrFenced
			}
			return bridge.AcquireAdmission(c)
		},
		Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
			if h.Credential != "allowed" {
				return "", ErrStreamAuthorization
			}
			return "access-session", nil
		},
		ServeStream: func(c context.Context, _ streamauth.Header, n net.Conn) error {
			if associations.Add(1) != 1 {
				return ServeHTTPConnection(c, n, resource)
			}
			return ServeHTTPConnection(c, &nativeHTTPFlushGateConn{Conn: n, ctx: c, entered: flushEntered, allow: flushAllow, closed: make(chan struct{})}, resource)
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second, Active: func(context.Context) (hostdproto.Status, error) { return active, nil }})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := streamauth.New("operation", "file_transfer", "stream", "allowed", time.Now().Add(time.Minute), 1<<20)
	conn, err := client.Open(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	reader := bufio.NewReader(conn)
	read := func() *http.Response {
		r, e := http.ReadResponse(reader, nil)
		if e != nil {
			t.Fatal(e)
		}
		_, e = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	_, _ = io.WriteString(conn, "GET /policy HTTP/1.1\r\nHost: local\r\n\r\n")
	select {
	case <-flushEntered:
	case <-ctx.Done():
		t.Fatal("response flush never started")
	}
	flushing, flushCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = controller.PrepareMaintenance(flushing, false, nil)
	flushCancel()
	if !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		t.Fatalf("native admission ended before final response flush: %v", err)
	}
	allowFlush()
	if read().StatusCode != 200 {
		t.Fatal("policy failed")
	}
	<-entered
	idle, idleCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = controller.PrepareMaintenance(idle, false, nil)
	idleCancel()
	if err != nil {
		t.Fatalf("idle HTTP association blocks maintenance: %v", err)
	}
	controller.AbortMaintenance()
	_, _ = io.WriteString(conn, "POST /payload HTTP/1.1\r\nHost: local\r\nContent-Length: 2\r\n\r\na")
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	interrupted, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	_, err = controller.PrepareMaintenance(interrupted, false, nil)
	stop()
	if !errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		t.Fatalf("incomplete payload not protected: %v", err)
	}
	release, err := controller.AcquireActive(active.WorkerID, active.Epoch)
	if err != nil {
		t.Fatalf("canceled preparation did not reopen: %v", err)
	}
	release()
	prepared := make(chan error, 1)
	go func() { _, e := controller.PrepareMaintenance(ctx, false, nil); prepared <- e }()
	select {
	case e := <-prepared:
		t.Fatalf("payload admitted early: %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(conn, "b")
	if read().StatusCode != 200 {
		t.Fatal("payload failed")
	}
	select {
	case e := <-prepared:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("completed response retained lease")
	}
	controller.AbortMaintenance()

	// A client disappearing mid-body releases its request admission as well.
	abandoned, err := client.Open(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(abandoned, "POST /payload HTTP/1.1\r\nHost: local\r\nContent-Length: 2\r\n\r\na")
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_ = abandoned.Close()
	cleanup, cleanupCancel := context.WithTimeout(ctx, time.Second)
	_, err = controller.PrepareMaintenance(cleanup, false, nil)
	cleanupCancel()
	if err != nil {
		t.Fatalf("abandoned payload retained admission: %v", err)
	}
	controller.AbortMaintenance()
	_ = activate("replacement")
	_, _ = io.WriteString(conn, "GET /policy HTTP/1.1\r\nHost: local\r\n\r\n")
	if r := read(); r.StatusCode != 503 || !r.Close {
		t.Fatalf("old association not fenced: %d close=%v", r.StatusCode, r.Close)
	}
	if calls.Load() != 3 {
		t.Fatal("fenced request reached resource")
	}
}

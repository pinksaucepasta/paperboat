package server

import (
	"bytes"
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

func TestNativeDispatchAuthorizesBeforeForwardingAndPreservesHalfClose(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer func() { _ = diagnosticlog.Flush(t.Context()); slog.SetDefault(previous) }()
	// Accepted long-running work must outlive the setup credential deadline.
	for _, consumer := range []string{"terminal", "exec", "file_transfer"} {
		t.Run(consumer, func(t *testing.T) { testNativeDispatchAcceptedStream(t, consumer) })
	}
	if err := diagnosticlog.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"reason":"credential_binding_invalid"`) {
		t.Fatal("missing typed target refusal reason")
	}
	if strings.Contains(logs.String(), "private-refusal-detail") {
		t.Fatal("refusal diagnostics retained private error text")
	}
}

func testNativeDispatchAcceptedStream(t *testing.T, consumer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var opened atomic.Int32
	lost := make(chan struct{})
	released := make(chan struct{}, 4)
	var fenced atomic.Bool
	var fencedCalls atomic.Int32
	staleCtx, stopStale := context.WithCancel(ctx)
	defer stopStale()
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(context.Context) (func(), <-chan struct{}, error) {
			if fenced.Load() {
				// The stale caller gives up while its retry is in flight, before this refusal is answered.
				if fencedCalls.Add(1) == 2 {
					stopStale()
				}
				return nil, nil, hostdproto.ErrFenced
			}
			return func() { released <- struct{}{} }, lost, nil
		},
		Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
			if h.Credential != "allowed" {
				return "", &auth.Error{Code: auth.BindingInvalid, Cause: errors.New("private-refusal-detail")}
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
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
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
	// Isolate the fenced worker from the expired-credential check above.
	allowed.DeadlineUnix = time.Now().Add(2 * time.Second).Unix()
	fenced.Store(true)
	staleConnection, staleErr := client.Open(staleCtx, allowed)
	if staleConnection != nil {
		staleConnection.Close()
	}
	if !errors.Is(staleErr, hostdproto.ErrFenced) {
		t.Fatalf("stale=%v", staleErr)
	}
	wrong, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("b", 43), DialTimeout: time.Second})
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
		Fence: func(context.Context) (func(), <-chan struct{}, error) { return func() {}, lost, nil }, Authorize: func(context.Context, streamauth.Header) (string, error) { return "access", nil },
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
	client, _ := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
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

func TestNativeDispatchKeepsAdmissionDuringWorkerMaintenance(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	controller, err := hostdproto.NewController(hostdproto.ControllerConfig{APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	welcome, err := controller.Negotiate(hostdproto.Hello{WorkerID: "worker", Version: "test", APIMin: 1, APIMax: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.MarkReady(hostdproto.Ready{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
		t.Fatal(err)
	}
	if _, err = controller.Activate(hostdproto.Activate{WorkerID: welcome.WorkerID, APIVersion: welcome.APIVersion, Epoch: welcome.Epoch, Lease: welcome.Lease}); err != nil {
		t.Fatal(err)
	}
	var opened atomic.Int32
	lost := make(chan struct{})
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(context.Context) (func(), <-chan struct{}, error) {
			release, err := controller.AcquireActive(welcome.WorkerID, welcome.Epoch)
			return release, lost, err
		},
		Authorize: func(_ context.Context, h streamauth.Header) (string, error) {
			if h.Credential != "allowed" {
				return "", &auth.Error{Code: auth.ScopeInvalid}
			}
			return "access-session", nil
		},
		ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error {
			opened.Add(1)
			_, err := io.Copy(c, c)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h, err := streamauth.New("operation", "terminal", "stream", "allowed", time.Now().Add(5*time.Second), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = controller.PrepareMaintenance(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	_, err = client.Authorize(ctx, h)
	var refusal *native.AdmissionRefusal
	if !errors.As(err, &refusal) || refusal.Code != native.AdmissionUnavailable {
		t.Error("maintenance refusal lost its retryable type")
	}
	controller.AbortMaintenance()
	if _, err = client.Authorize(ctx, h); err != nil {
		t.Fatal(err)
	}
	// The owner can enter maintenance after authorization but before the stream opens.
	if _, err = controller.PrepareMaintenance(ctx, false, nil); err != nil {
		t.Fatal(err)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() { c, e := client.Open(ctx, h); done <- result{c, e} }()
	select {
	case r := <-done:
		if r.conn != nil {
			r.conn.Close()
		}
		t.Fatal("worker maintenance escaped an in-flight stream admission")
	case <-time.After(200 * time.Millisecond):
	}
	if opened.Load() != 0 {
		t.Fatal("application ran before native commit")
	}
	controller.AbortMaintenance()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		r.conn.Close()
	case <-ctx.Done():
		t.Fatal("stream admission did not resume after native commit")
	}
	denied := h
	denied.Credential = "denied"
	_, err = client.Authorize(ctx, denied)
	if !errors.Is(err, ErrStreamAuthorization) || !errors.As(err, &refusal) || refusal.Code != native.AdmissionDenied {
		t.Fatal("permanent authorization rejection became retryable")
	}
}

// A terminal resume carries no credential, so the carrier vouches for the peer it admitted. The worker must
// serve it as a ResumePeerConn for exactly that peer, never through the credential check, and refuse a resume
// header that does not name the carrier's peer.
func TestNativeDispatchResumeStreamCarriesTheCarrierProvenPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	served := make(chan ResumePeerConn, 1)
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(context.Context) (func(), <-chan struct{}, error) { return func() {}, make(chan struct{}), nil },
		Authorize: func(context.Context, streamauth.Header) (string, error) {
			return "", errors.New("a resume must not reach the credential check")
		},
		AuthorizeResume: func(_ context.Context, h streamauth.Header) (string, error) { return h.Resume, nil },
		ServeStream: func(_ context.Context, _ streamauth.Header, conn net.Conn) error {
			resume, ok := conn.(ResumePeerConn)
			if !ok {
				return errors.New("worker stream is not a ResumePeerConn")
			}
			served <- resume
			_, err := io.Copy(io.Discard, conn)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.NewResume("terminal-session", "stream", "access-session", time.Now().Add(2*time.Second), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, header); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("a resume without the carrier's peer was admitted: %v", err)
	}
	connection, err := client.OpenResume(ctx, header, NativeDispatchResumePeer{AccountID: "account", EndpointID: "endpoint", Standing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case resume := <-served:
		account, endpoint, session, resource, standing := resume.ResumePeer()
		if account != "account" || endpoint != "endpoint" || session != "terminal-session" || resource != "access-session" || !standing {
			t.Fatalf("peer=%q/%q session=%q resource=%q standing=%v", account, endpoint, session, resource, standing)
		}
	case <-ctx.Done():
		t.Fatal("resume stream never reached the worker's terminal server")
	}
	credentialed, _ := streamauth.New("operation", "terminal", "stream", "allowed", time.Now().Add(2*time.Second), 1<<20)
	if _, err := client.OpenResume(ctx, credentialed, NativeDispatchResumePeer{AccountID: "account", EndpointID: "endpoint"}); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("a credentialed stream carried a resume peer: %v", err)
	}
}

// The replay ticket's deadline bounds admission only: an admitted replay keeps streaming after it,
// while a config comparison still ends at its header deadline.
func TestNativeDispatchReplayOutlivesItsTicketDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ended := make(chan string, 2)
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 2, HandshakeTimeout: time.Second,
		Fence: func(context.Context) (func(), <-chan struct{}, error) { return func() {}, nil, nil }, Authorize: func(context.Context, streamauth.Header) (string, error) { return "access", nil },
		ServeStream: func(ctx context.Context, header streamauth.Header, _ net.Conn) error {
			select {
			case <-ctx.Done():
				ended <- header.Consumer
			case <-time.After(4 * time.Second):
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, _ := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
	deadline := time.Now().Add(2 * time.Second)
	for _, consumer := range []string{"recording", "config_compare"} {
		header, _ := streamauth.New("operation-"+consumer, consumer, "stream-"+consumer, "allowed", deadline, 1<<20)
		conn, err := client.Open(ctx, header)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
	}
	select {
	case consumer := <-ended:
		if consumer != "config_compare" {
			t.Fatalf("%s ended at its header deadline", consumer)
		}
	case <-time.After(3500 * time.Millisecond):
		t.Fatal("the comparison outlived its header deadline")
	}
	select {
	case consumer := <-ended:
		t.Fatalf("%s ended before its work finished", consumer)
	case <-time.After(time.Second):
	}
}

// F21-11: a carrier that admitted an offline terminal or exec session from its signed scopes forwards the admitted
// peer, and the worker serves the stream as that peer's OfflinePeerConn (the terminal and exec servers authorize the
// one session from it). The peer and the offline header come together or not at all, only for terminal and exec.
func TestNativeDispatchCarriesAnOfflineSessionsPeerToTheWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	served := make(chan OfflinePeerConn, 1)
	handler, err := NewNativeDispatchHandler(NativeDispatchConfig{Token: strings.Repeat("a", 43), MaximumConcurrent: 4, HandshakeTimeout: time.Second,
		Fence: func(context.Context) (func(), <-chan struct{}, error) { return func() {}, make(chan struct{}), nil },
		Authorize: func(context.Context, streamauth.Header) (string, error) {
			return "", errors.New("an offline session must not reach the credential check")
		},
		AuthorizeOffline: func(_ context.Context, h streamauth.Header) (string, error) { return h.OperationID, nil },
		ServeStream: func(_ context.Context, _ streamauth.Header, conn net.Conn) error {
			offline, ok := conn.(OfflinePeerConn)
			if !ok {
				return errors.New("worker stream is not an OfflinePeerConn")
			}
			served <- offline
			_, err := io.Copy(io.Discard, conn)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer handler.Close(ctx)
	worker := httptest.NewServer(handler)
	defer worker.Close()
	client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	header, err := streamauth.NewOffline("operation-offline", "exec", "stream", time.Now().Add(2*time.Second), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, header); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("an offline session without the carrier's peer was admitted: %v", err)
	}
	connection, err := client.OpenOffline(ctx, header, NativeDispatchOfflinePeer{AccountID: "account", EndpointID: "endpoint"})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	select {
	case offline := <-served:
		account, endpoint, consumer, operation, deadline := offline.OfflinePeer()
		if account != "account" || endpoint != "endpoint" || consumer != "exec" || operation != "operation-offline" || !deadline.After(time.Now()) {
			t.Fatalf("peer=%q/%q consumer=%q operation=%q deadline=%v", account, endpoint, consumer, operation, deadline)
		}
	case <-ctx.Done():
		t.Fatal("offline stream never reached the worker's exec server")
	}
	credentialed, _ := streamauth.New("operation", "exec", "stream", "allowed", time.Now().Add(2*time.Second), 1<<20)
	if _, err := client.OpenOffline(ctx, credentialed, NativeDispatchOfflinePeer{AccountID: "account", EndpointID: "endpoint"}); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("a credentialed stream carried an offline peer: %v", err)
	}
	if _, err := client.OpenOffline(ctx, header, NativeDispatchOfflinePeer{AccountID: "account"}); !errors.Is(err, ErrStreamAuthorization) {
		t.Fatalf("an offline peer without an endpoint was admitted: %v", err)
	}
}

// The offline forms the worker must refuse: no gate configured, a session a host never starts offline, both peer
// envelopes at once, and a terminal that is allowed (its happy path reaches the worker like exec's).
func TestNativeDispatchRefusesWhatMayNotStartOffline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	newClient := func(config NativeDispatchConfig) (*NativeDispatchClient, func()) {
		config.Token, config.MaximumConcurrent, config.HandshakeTimeout = strings.Repeat("a", 43), 4, time.Second
		config.Fence = func(context.Context) (func(), <-chan struct{}, error) { return func() {}, make(chan struct{}), nil }
		config.Authorize = func(context.Context, streamauth.Header) (string, error) { return "", errors.New("credential check") }
		handler, err := NewNativeDispatchHandler(config)
		if err != nil {
			t.Fatal(err)
		}
		worker := httptest.NewServer(handler)
		client, err := NewNativeDispatchClient(NativeDispatchClientConfig{Address: strings.TrimPrefix(worker.URL, "http://"), Token: strings.Repeat("a", 43), DialTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return client, func() { worker.Close(); _ = handler.Close(ctx) }
	}
	peer := NativeDispatchOfflinePeer{AccountID: "account", EndpointID: "endpoint"}
	serve := func(_ context.Context, _ streamauth.Header, conn net.Conn) error {
		_, err := io.Copy(io.Discard, conn)
		return err
	}

	unset, closeUnset := newClient(NativeDispatchConfig{ServeStream: serve})
	defer closeUnset()
	exec, _ := streamauth.NewOffline("op-exec", "exec", "stream", time.Now().Add(2*time.Second), 1<<20)
	if _, err := unset.OpenOffline(ctx, exec, peer); err == nil {
		t.Fatal("an offline exec was admitted without an AuthorizeOffline gate")
	}

	served := make(chan string, 2)
	gated, closeGated := newClient(NativeDispatchConfig{ServeStream: func(c context.Context, h streamauth.Header, conn net.Conn) error {
		served <- h.Consumer
		return serve(c, h, conn)
	}, AuthorizeOffline: func(_ context.Context, h streamauth.Header) (string, error) { return h.OperationID, nil }})
	defer closeGated()
	ssh, _ := streamauth.NewOffline("op-ssh", "ssh", "stream", time.Now().Add(2*time.Second), 1<<20)
	if _, err := gated.OpenOffline(ctx, ssh, peer); err == nil {
		t.Fatal("an offline ssh stream was dispatched to the worker")
	}
	terminal, _ := streamauth.NewOffline("op-terminal", "terminal", "stream", time.Now().Add(2*time.Second), 1<<20)
	connection, err := gated.OpenOffline(ctx, terminal, peer)
	if err != nil {
		t.Fatalf("an allowed offline terminal was refused: %v", err)
	}
	defer connection.Close()
	select {
	case consumer := <-served:
		if consumer != "terminal" {
			t.Fatalf("worker served %q", consumer)
		}
	case <-ctx.Done():
		t.Fatal("offline terminal never reached the worker")
	}
}

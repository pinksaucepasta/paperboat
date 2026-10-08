package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type localPeerRemote struct {
	net.Conn
	mu             sync.Mutex
	rows, cols     uint16
	closeOnWait    bool
	runtimeVersion string
}

func (c *localPeerRemote) Resize(rows, cols uint16) error {
	c.mu.Lock()
	c.rows, c.cols = rows, cols
	c.mu.Unlock()
	return nil
}
func (c *localPeerRemote) Wait() (int, error) {
	if c.closeOnWait {
		_ = c.Conn.Close()
	}
	return 7, nil
}
func (c *localPeerRemote) CloseWrite() error              { return nil }
func (c *localPeerRemote) TerminalRuntimeVersion() string { return c.runtimeVersion }

type localPeerExecRemote struct {
	*localPeerRemote
	events              chan ExecEvent
	cancelled, detached bool
	signal              string
}

type cursorPeerRemote struct {
	*localPeerRemote
	cursor *LocalPeerCursorBridge
	reads  int
}

func (c *cursorPeerRemote) Read(value []byte) (int, error) {
	c.reads++
	switch c.reads {
	case 1:
		copy(value, "abcdef")
		c.cursor.RecordSequence(6)
		return 6, nil
	default:
		c.cursor.RecordSequence(9)
		return 0, io.EOF
	}
}

func TestLocalPeerCursorAdvancesOnlyAfterOutputConsumption(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	defer remotePeer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cursor := &LocalPeerCursorBridge{}
	cursor.RecordSequence(2)
	cursor.RecordReplayGap(1, 2, 2)
	served := make(chan error, 1)
	go func() {
		served <- ServeLocalPeerTerminalConn(ctx, localServer, &cursorPeerRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, cursor: cursor}, cursor)
	}()
	sequences := make(chan int, 4)
	gaps := make(chan localPeerReplayGapPayload, 1)
	connection, err := NewLocalPeerConn(localClient, func(sequence int) { sequences <- sequence }, func(requested, earliest, latest uint64) {
		gaps <- localPeerReplayGapPayload{requested, earliest, latest}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := connection.Read(nil); n != 0 || err != nil || len(sequences) != 0 {
		t.Fatalf("zero read=(%d,%v) pending sequences=%d", n, err, len(sequences))
	}
	buffer := make([]byte, 1)
	for index := 0; index < 5; index++ {
		if _, err := io.ReadFull(connection, buffer); err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			if sequence := <-sequences; sequence != 2 {
				t.Fatalf("baseline sequence=%d", sequence)
			}
		}
		if len(sequences) != 0 {
			t.Fatalf("cursor advanced before data drained")
		}
	}
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatal(err)
	}
	if sequence := <-sequences; sequence != 6 {
		t.Fatalf("data sequence=%d", sequence)
	}
	readDone := make(chan error, 1)
	go func() { _, readErr := connection.Read(buffer); readDone <- readErr }()
	select {
	case sequence := <-sequences:
		if sequence != 9 {
			t.Fatalf("final sequence=%d", sequence)
		}
	case <-time.After(time.Second):
		t.Fatal("final sequence was not delivered")
	}
	cancel()
	select {
	case readErr := <-readDone:
		if !errors.Is(readErr, io.EOF) {
			t.Fatalf("final read error=%v", readErr)
		}
	case <-time.After(time.Second):
		t.Fatal("final read did not unblock")
	}
	select {
	case gap := <-gaps:
		if gap != (localPeerReplayGapPayload{1, 2, 2}) {
			t.Fatalf("gap=%v", gap)
		}
	default:
		t.Fatal("gap was not delivered")
	}
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop")
	}
}

func TestLocalPeerDebugConnCarriesCursor(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	defer remotePeer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cursor := &LocalPeerCursorBridge{}
	cursor.RecordSequence(2)
	served := make(chan error, 1)
	go func() {
		remote := &cursorPeerRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer, runtimeVersion: "runtime-1"}, cursor: cursor}
		served <- ServeLocalPeerDebugTerminalConn(ctx, localServer, remote, cursor)
	}()
	sequences := []int{}
	connection, err := newLocalPeerDebugConn(localClient, func(sequence int) { sequences = append(sequences, sequence) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TerminalRuntimeVersion(connection); got != "runtime-1" {
		t.Fatalf("runtime version=%q", got)
	}
	output := make([]byte, 6)
	if _, err := io.ReadFull(connection, output); err != nil || string(output) != "abcdef" {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if len(sequences) != 2 || sequences[0] != 2 || sequences[1] != 6 {
		t.Fatalf("sequences=%v", sequences)
	}
	cancel()
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("debug bridge did not stop")
	}
}

func (c *localPeerExecRemote) Events() <-chan ExecEvent { return c.events }
func (c *localPeerExecRemote) Cancel() error {
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()
	return nil
}
func (c *localPeerExecRemote) Signal(signal string) error {
	c.mu.Lock()
	c.signal = signal
	c.mu.Unlock()
	return nil
}
func (c *localPeerExecRemote) Detach() error {
	c.mu.Lock()
	c.detached = true
	c.mu.Unlock()
	return nil
}

func TestLocalPeerConnectionsPreserveRuntimeVersion(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &localPeerRemote{Conn: remoteServer, runtimeVersion: "2026.08.27.65"}
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerDebugConn(context.Background(), localServer, remote) }()
	connection, err := newLocalPeerDebugConn(localClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TerminalRuntimeVersion(connection); got != "2026.08.27.65" {
		t.Fatalf("local runtime version=%q", got)
	}
	_ = connection.Close()
	_ = remotePeer.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local peer server did not stop")
	}
}

func TestLocalPeerDebugConnPreservesFirstFrameFromDaemonWithoutMetadata(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- ServeLocalPeerConn(context.Background(), localServer, &localPeerRemote{Conn: remoteServer})
	}()
	go func() { _, _ = remotePeer.Write([]byte("banner")) }()
	connection, err := newLocalPeerDebugConn(localClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := TerminalRuntimeVersion(connection); got != "" {
		t.Fatalf("runtime version=%q", got)
	}
	buffer := make([]byte, len("banner"))
	if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "banner" {
		t.Fatalf("first frame=%q err=%v", buffer, err)
	}
	_ = connection.Close()
	_ = remotePeer.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local peer server did not stop")
	}
}

func TestLocalPeerConnPreservesDataResizeAndWait(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &localPeerRemote{Conn: remoteServer}
	remote.closeOnWait = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(ctx, localServer, remote) }()
	connection, err := NewLocalPeerConn(localClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := connection.(ExecConn); ok {
		t.Fatal("ordinary local connection exposed exec controls")
	}
	defer connection.Close()
	defer remotePeer.Close()
	if _, err := connection.Write([]byte("input")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if _, err := remotePeer.Read(buffer); err != nil || string(buffer) != "input" {
		t.Fatalf("input=%q err=%v", buffer, err)
	}
	go func() { _, _ = remotePeer.Write([]byte("output")) }()
	buffer = make([]byte, 6)
	if _, err := connection.Read(buffer); err != nil || string(buffer) != "output" {
		t.Fatalf("output=%q err=%v", buffer, err)
	}
	if err := connection.Resize(24, 80); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		remote.mu.Lock()
		rows, cols := remote.rows, remote.cols
		remote.mu.Unlock()
		if rows == 24 && cols == 80 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	remote.mu.Lock()
	rows, cols := remote.rows, remote.cols
	remote.mu.Unlock()
	if rows != 24 || cols != 80 {
		t.Fatalf("resize=%dx%d", rows, cols)
	}
	if code, err := connection.Wait(); err != nil || code != 7 {
		t.Fatalf("wait=%d err=%v", code, err)
	}
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local peer server did not stop")
	}
}

type blockingWaitRemote struct {
	*localPeerRemote
	release chan struct{}
}

type blockingWaitExecRemote struct {
	*localPeerExecRemote
	release chan struct{}
}

func (r *blockingWaitExecRemote) Wait() (int, error) {
	<-r.release
	return 0, nil
}

func (r *blockingWaitRemote) Wait() (int, error) {
	<-r.release
	_ = r.Conn.Close()
	return 0, nil
}

func TestServeLocalPeerConnWaitDoesNotBlockControlFrames(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &blockingWaitRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(ctx, localServer, remote) }()
	connection, err := NewLocalPeerConn(localClient, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer remotePeer.Close()

	waitDone := make(chan struct{})
	go func() {
		_, _ = connection.Wait()
		close(waitDone)
	}()
	if err := connection.Resize(31, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("input-after-wait")); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len("input-after-wait"))
	if _, err := io.ReadFull(remotePeer, buffer); err != nil || string(buffer) != "input-after-wait" {
		t.Fatalf("input=%q err=%v", buffer, err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		remote.mu.Lock()
		rows, cols := remote.rows, remote.cols
		remote.mu.Unlock()
		if rows == 31 && cols == 101 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	remote.mu.Lock()
	rows, cols := remote.rows, remote.cols
	remote.mu.Unlock()
	if rows != 31 || cols != 101 {
		t.Fatalf("resize=%dx%d", rows, cols)
	}
	close(remote.release)
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("wait did not complete")
	}
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local peer server did not stop")
	}
}

func TestLocalExecPeerConnPreservesEventsAndControls(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &localPeerExecRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, events: make(chan ExecEvent, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(ctx, localServer, remote) }()
	connection, err := NewLocalExecPeerConn(localClient)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer remotePeer.Close()
	remote.events <- ExecEvent{OperationID: "operation_1", EventSequence: 1, Stream: "stderr", Data: []byte("failure")}
	select {
	case event := <-connection.Events():
		if event.OperationID != "operation_1" || event.Stream != "stderr" || string(event.Data) != "failure" {
			t.Fatalf("event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("exec event timed out")
	}
	if err := connection.Cancel(); err != nil {
		t.Fatal(err)
	}
	if err := connection.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	if err := connection.Detach(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		remote.mu.Lock()
		ready := remote.cancelled && remote.detached && remote.signal == "TERM"
		remote.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	remote.mu.Lock()
	cancelled, detached, signal := remote.cancelled, remote.detached, remote.signal
	remote.mu.Unlock()
	if !cancelled || !detached || signal != "TERM" {
		t.Fatalf("cancelled=%t detached=%t signal=%q", cancelled, detached, signal)
	}
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local exec server did not stop")
	}
}

func TestLocalExecPeerConnWaitSurvivesRemoteEOFAfterTerminalEvent(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &localPeerExecRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, events: make(chan ExecEvent, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(ctx, localServer, remote) }()
	connection, err := NewLocalExecPeerConn(localClient)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	remote.events <- ExecEvent{OperationID: "operation_1", Stream: "stdout", Data: []byte("out")}
	remote.events <- ExecEvent{OperationID: "operation_1", Stream: "stderr", Data: []byte("err")}
	remote.events <- ExecEvent{OperationID: "operation_1", State: "exited", Result: &ExecResult{Code: 7}}
	want := []ExecEvent{{Stream: "stdout", Data: []byte("out")}, {Stream: "stderr", Data: []byte("err")}, {State: "exited", Result: &ExecResult{Code: 7}}}
	for _, expected := range want {
		select {
		case event := <-connection.Events():
			if event.Stream != expected.Stream || event.State != expected.State || string(event.Data) != string(expected.Data) || expected.Result != nil && (event.Result == nil || event.Result.Code != expected.Result.Code) {
				t.Fatalf("event=%+v expected=%+v", event, expected)
			}
		case <-time.After(time.Second):
			t.Fatal("exec event timed out")
		}
	}
	_ = remotePeer.Close()
	if code, err := connection.Wait(); err != nil || code != 7 {
		t.Fatalf("wait=%d err=%v", code, err)
	}
	_ = connection.Close()
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("local exec server did not stop")
	}
}

func TestLocalExecPeerConnFailsWhenEventsCloseWithoutTerminalOutcome(t *testing.T) {
	localClient, localServer := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	remote := &blockingWaitExecRemote{
		localPeerExecRemote: &localPeerExecRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, events: make(chan ExecEvent)},
		release:             make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(ctx, localServer, remote) }()
	connection, err := NewLocalExecPeerConn(localClient)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	defer remotePeer.Close()

	close(remote.events)
	select {
	case _, ok := <-connection.Events():
		if ok {
			t.Fatal("events remained open after remote event stream closed")
		}
	case <-time.After(time.Second):
		t.Fatal("events did not close")
	}
	waitDone := make(chan error, 1)
	go func() {
		_, waitErr := connection.Wait()
		waitDone <- waitErr
	}()
	select {
	case waitErr := <-waitDone:
		if !errors.Is(waitErr, ErrTransportLost) {
			t.Fatalf("wait error=%v, want transport loss", waitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("wait blocked after remote event stream closed")
	}
	select {
	case serveErr := <-served:
		if !errors.Is(serveErr, ErrTransportLost) {
			t.Fatalf("serve error=%v, want transport loss", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after remote event stream closed")
	}
}

func TestLocalExecAbortJoinsSaturatedReaderAndAllowsFreshAttachment(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		client, server := net.Pipe()
		connection, err := NewLocalExecPeerConn(client)
		if err != nil {
			t.Fatal(err)
		}
		peer := connection.(*localPeerConn)
		t.Cleanup(func() { _ = peer.Abort(); _ = server.Close() })
		written := make(chan struct{})
		go func() {
			defer close(written)
			writer := &localPeerWriter{writer: server}
			payload, _ := json.Marshal(ExecEvent{OperationID: "same-operation", Stream: "stdout", Data: []byte("x")})
			for i := 0; i < 300; i++ {
				if writer.write(localPeerExecEvent, payload) != nil {
					return
				}
			}
		}()
		deadline := time.Now().Add(time.Second)
		for len(peer.events) < cap(peer.events) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(peer.events) != cap(peer.events) {
			t.Fatal("reader did not reach real bounded event queue")
		}
		done := make(chan error, 1)
		go func() { done <- peer.Abort() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("abort did not join saturated reader")
		}
		select {
		case <-peer.done:
		default:
			t.Fatal("reader remains alive")
		}
		select {
		case <-written:
		case <-time.After(time.Second):
			t.Fatal("peer writer remains alive")
		}
		if err := peer.Abort(); err != nil {
			t.Fatal(err)
		}
		_ = server.Close()
	}
}

type abortExecRemote struct {
	*localPeerExecRemote
	closeCalls int
}

func (r *abortExecRemote) Close() error {
	r.mu.Lock()
	r.closeCalls++
	r.mu.Unlock()
	return r.localPeerRemote.Close()
}

func TestLocalExecAttachmentLossDetachesWithoutRemoteCancel(t *testing.T) {
	client, server := net.Pipe()
	remoteServer, remotePeer := net.Pipe()
	defer remotePeer.Close()
	remote := &abortExecRemote{localPeerExecRemote: &localPeerExecRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, events: make(chan ExecEvent)}}
	served := make(chan error, 1)
	go func() { served <- ServeLocalPeerConn(context.Background(), server, remote) }()
	connection, err := NewLocalExecPeerConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.(*localPeerConn).Abort(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("bridge workers did not join")
	}
	remote.mu.Lock()
	detached, canceled, closes := remote.detached, remote.cancelled, remote.closeCalls
	remote.mu.Unlock()
	if !detached || canceled || closes != 0 {
		t.Fatalf("detach=%v cancel=%v close=%d", detached, canceled, closes)
	}
	_ = remoteServer.Close()
}

type acknowledgedExecRemote struct {
	*localPeerExecRemote
	entered chan struct{}
	release chan struct{}
	failure error
}

func (r *acknowledgedExecRemote) Cancel() error {
	close(r.entered)
	<-r.release
	return r.failure
}
func TestLocalExecCancelWaitsForActualRemoteAcknowledgment(t *testing.T) {
	for _, failure := range []error{nil, io.ErrUnexpectedEOF} {
		client, server := net.Pipe()
		remoteServer, remotePeer := net.Pipe()
		remote := &acknowledgedExecRemote{localPeerExecRemote: &localPeerExecRemote{localPeerRemote: &localPeerRemote{Conn: remoteServer}, events: make(chan ExecEvent)}, entered: make(chan struct{}), release: make(chan struct{}), failure: failure}
		served := make(chan error, 1)
		go func() { served <- ServeLocalPeerConn(context.Background(), server, remote) }()
		connection, err := NewLocalExecPeerConn(client)
		if err != nil {
			t.Fatal(err)
		}
		canceled := make(chan error, 1)
		go func() { canceled <- connection.Cancel() }()
		select {
		case <-remote.entered:
		case <-time.After(time.Second):
			t.Fatal("cancel did not reach remote owner")
		}
		select {
		case <-canceled:
			t.Fatal("frame write falsely confirmed remote cancellation")
		default:
		}
		close(remote.release)
		select {
		case err := <-canceled:
			if (err == nil) != (failure == nil) {
				t.Fatalf("cancel result=%v remote failure=%v", err, failure)
			}
		case <-time.After(time.Second):
			t.Fatal("cancel acknowledgment did not complete")
		}
		_ = connection.(*localPeerConn).Abort()
		select {
		case err := <-served:
			if failure != nil && !errors.Is(err, failure) {
				t.Fatal("remote original cause lost")
			}
		case <-time.After(time.Second):
			t.Fatal("bridge did not join")
		}
		_ = remoteServer.Close()
		_ = remotePeer.Close()
	}
}

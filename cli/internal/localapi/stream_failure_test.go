package localapi

import (
	"context"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

type trackedStreamReader struct {
	net.Conn
	started, returned chan struct{}
	start, finish     sync.Once
}

func (c *trackedStreamReader) Read(p []byte) (int, error) {
	c.start.Do(func() { close(c.started) })
	n, err := c.Conn.Read(p)
	c.finish.Do(func() { close(c.returned) })
	return n, err
}

func TestPeerStreamCancellationJoinsBothCopyWorkers(t *testing.T) {
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	defer aPeer.Close()
	defer bPeer.Close()
	local := &trackedStreamReader{Conn: a, started: make(chan struct{}), returned: make(chan struct{})}
	remote := &trackedStreamReader{Conn: b, started: make(chan struct{}), returned: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	done := make(chan struct{})
	go func() { bridgePeerStream(ctx, local, remote); close(done) }()
	for _, started := range []<-chan struct{}{local.started, remote.started} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("copy worker did not begin")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled stream did not finish")
	}
	for _, returned := range []<-chan struct{}{local.returned, remote.returned} {
		select {
		case <-returned:
		default:
			t.Fatal("stream returned before joining its reader")
		}
	}
	if len(faults) != 0 {
		t.Fatal("ordinary canceled stream emitted a failure")
	}
}

type failingStreamReader struct{ net.Conn }

func (*failingStreamReader) Read([]byte) (int, error) { return 0, syscall.EIO }

func TestPeerStreamFailureIsCorrelatedAndFreshStreamRecovers(t *testing.T) {
	ref := supportref.New()
	ctx := supportref.WithContext(context.Background(), ref)
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, f errorreport.Fault) { faults = append(faults, f) })
	defer restore()
	a, aPeer := net.Pipe()
	b, bPeer := net.Pipe()
	bridgePeerStream(ctx, a, &failingStreamReader{Conn: b})
	aPeer.Close()
	bPeer.Close()
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != ref || faults[0].Stage != "delivery" {
		t.Fatal("stream failure lost its original cause or correlation")
	}
	a, aPeer = net.Pipe()
	b, bPeer = net.Pipe()
	defer aPeer.Close()
	defer bPeer.Close()
	_ = aPeer.SetDeadline(time.Now().Add(time.Second))
	_ = bPeer.SetDeadline(time.Now().Add(time.Second))
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { bridgePeerStream(streamCtx, a, b); close(done) }()
	writeDone := make(chan error, 1)
	go func() { _, err := aPeer.Write([]byte{0, 1, 2, 255}); writeDone <- err }()
	var payload [4]byte
	if _, err := io.ReadFull(bPeer, payload[:]); err != nil || payload != [4]byte{0, 1, 2, 255} {
		t.Fatal("fresh stream did not carry unchanged binary bytes")
	}
	if err := <-writeDone; err != nil {
		t.Fatal("fresh stream write failed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("healthy stream cleanup did not finish")
	}
	if len(faults) != 1 {
		t.Fatal("healthy recovery or normal cleanup emitted another failure")
	}
}

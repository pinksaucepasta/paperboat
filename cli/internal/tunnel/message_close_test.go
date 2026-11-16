package tunnel

import (
	"context"
	"testing"
	"time"
)

type closeRecordingStream struct{ halfClosed, closed bool }

func (*closeRecordingStream) Read([]byte) (int, error)         { return 0, nil }
func (*closeRecordingStream) Write(p []byte) (int, error)      { return len(p), nil }
func (*closeRecordingStream) SetWriteDeadline(time.Time) error { return nil }
func (s *closeRecordingStream) CloseWrite() error              { s.halfClosed = true; return nil }
func (s *closeRecordingStream) Close() error                   { s.closed = true; return nil }

type nopStreamGroup struct{}

func (nopStreamGroup) OpenStream(context.Context) (nativeStream, error) { return nil, nil }
func (nopStreamGroup) Close() error                                     { return nil }

// F21-164: a closed terminal must fully close its streams, not only send the FIN. A FIN-only stream kept the host's unread
// output, which held the peer's shared QUIC connection window, and every other terminal on that peer stalled.
func TestNativeMessageCloseFullyClosesEveryStream(t *testing.T) {
	streams := []*closeRecordingStream{{}, {}, {}}
	ctx, cancel := context.WithCancel(context.Background())
	message := &nativeMessageConnection{connection: nopStreamGroup{}, control: streams[0], input: streams[1], output: streams[2],
		ctx: ctx, cancel: cancel}
	_ = message.Close()
	for i, s := range streams {
		if !s.halfClosed || !s.closed {
			t.Fatalf("stream %d: FIN sent %v, closed %v; want both", i, s.halfClosed, s.closed)
		}
	}
}

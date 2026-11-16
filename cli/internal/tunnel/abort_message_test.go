package tunnel

import (
	"context"
	"testing"
)

type abortRecorder struct {
	aborted, closed bool
}

func (*abortRecorder) ReadMessage(context.Context) (helperMessageType, []byte, error) {
	return 0, nil, nil
}
func (*abortRecorder) WriteMessage(context.Context, helperMessageType, []byte) error { return nil }
func (r *abortRecorder) Close() error                                                { r.closed = true; return nil }
func (r *abortRecorder) Abort()                                                      { r.aborted = true }

// A detached exec resets its native stream: every wrapper between helperExecConn and the stream must pass Abort on
// (pipelinedWelcome did not, so the stream was closed with a FIN behind unread stdin; device repro 2026-11-16).
func TestAbortMessageReachesTheStreamThroughEveryWrapper(t *testing.T) {
	for name, wrap := range map[string]func(helperMessageConnection) helperMessageConnection{
		"bare": func(m helperMessageConnection) helperMessageConnection { return m },
		"pipelined welcome": func(m helperMessageConnection) helperMessageConnection {
			return &pipelinedWelcome{helperMessageConnection: m}
		},
		"initial output": func(m helperMessageConnection) helperMessageConnection {
			return &initialOutputConnection{helperMessageConnection: m}
		},
		"initial over pipeline": func(m helperMessageConnection) helperMessageConnection {
			return &initialOutputConnection{helperMessageConnection: &pipelinedWelcome{helperMessageConnection: m}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := &abortRecorder{}
			if err := abortMessage(wrap(recorder)); err != nil {
				t.Fatal(err)
			}
			if !recorder.aborted || recorder.closed {
				t.Fatalf("aborted=%v closed=%v, want the stream reset and not closed", recorder.aborted, recorder.closed)
			}
		})
	}
}

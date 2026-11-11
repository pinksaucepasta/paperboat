package tunnel

import (
	"context"
	"sync"
	"sync/atomic"
)

// cursorTerminal relays one native terminal attachment and tracks the output
// cursor the local API acknowledges. It never reconnects by itself: a lost link
// returns to the CLI reconnect path.
type cursorTerminal struct {
	*ReconnectingConn
	outputMu     sync.Mutex
	delivered    uint64
	produced     uint64
	boundaries   []outputBoundary
	sequenceSink func(int)
	readMu       sync.Mutex
	writeMu      sync.Mutex
	helper       *helperTerminalConn
	sequence     *atomic.Int64
	observer     atomic.Pointer[func(bool)]
	recording    recordingState
}

func newCursorTerminal(ctx context.Context, helper *helperTerminalConn, sequence *atomic.Int64, sink func(int)) *cursorTerminal {
	c := &cursorTerminal{helper: helper, sequence: sequence, sequenceSink: sink}
	c.ReconnectingConn = NewObservedReconnectingConn(ctx, &cursorTerminalSource{helperTerminalConn: helper, owner: c}, 1, 0, nil, nil, nil, TelemetryContext{}, func(options *reconnectingOptions) {
		options.passive = true
		options.observer = func(event ReconnectEvent) {
			if event == ReconnectRecording || event == ReconnectNotRecording {
				c.recording.report(event == ReconnectRecording)
				return
			}
			if observer := c.observer.Load(); observer != nil {
				if event == ReconnectStalled {
					(*observer)(true)
				}
				if event == ReconnectResumed || event == ReconnectRecovered {
					(*observer)(false)
				}
			}
		}
	})
	return c
}

func (c *cursorTerminal) Close() error {
	c.cancel()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Close()
}

func (c *cursorTerminal) ObserveLinkState(observer func(bool)) { c.observer.Store(&observer) }
func (c *cursorTerminal) ObserveRecording(observer func(bool)) { c.recording.observe(observer) }
func (c *cursorTerminal) Write(data []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Write(data)
}
func (c *cursorTerminal) Resize(rows, columns uint16) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ReconnectingConn.Resize(rows, columns)
}
func (c *cursorTerminal) TerminalRuntimeVersion() string { return c.helper.TerminalRuntimeVersion() }
func (c *cursorTerminal) TerminalCompressionTelemetry() TerminalCompressionTelemetry {
	return c.helper.TerminalCompressionTelemetry()
}

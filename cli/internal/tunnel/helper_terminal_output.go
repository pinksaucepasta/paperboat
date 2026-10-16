package tunnel

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func (c *helperTerminalConn) decodeHelperBinary(data []byte) (helperOutput, error) {
	started := time.Now()
	frame, err := protocol.DecodeTerminalOutput(data)
	c.compressionDecodeNanos.Add(uint64(time.Since(started).Nanoseconds()))
	if err != nil || frame.StreamID != c.streamID {
		c.compressionDecodeFailures.Add(1)
		return helperOutput{}, errors.New("helper binary frame is invalid")
	}
	c.compressionDecodedBytes.Add(uint64(len(frame.Data)))
	c.compressionEncodedBytes.Add(uint64(len(data) - protocol.TerminalOutputHeaderBytes))
	if frame.Encoding == protocol.TerminalOutputZstd {
		c.compressionZstdFrames.Add(1)
	} else {
		c.compressionRawFrames.Add(1)
	}
	if frame.Channel == protocol.TerminalScreenCheckpoint {
		return c.decodeScreenCheckpoint(frame)
	}
	if c.checkpointActive || frame.Channel != protocol.TerminalStdout && frame.Channel != protocol.TerminalStderr {
		return helperOutput{}, errors.New("helper terminal output interrupted its screen checkpoint")
	}
	return helperOutput{data: frame.Data, endSequence: frame.StartSequence + uint64(len(frame.Data))}, nil
}

func (c *helperTerminalConn) TerminalCompressionTelemetry() TerminalCompressionTelemetry {
	return TerminalCompressionTelemetry{
		RawFrames: c.compressionRawFrames.Load(), ZstdFrames: c.compressionZstdFrames.Load(),
		DecodedBytes: c.compressionDecodedBytes.Load(), EncodedBytes: c.compressionEncodedBytes.Load(),
		DecodeNanos: c.compressionDecodeNanos.Load(), DecodeFailures: c.compressionDecodeFailures.Load(),
	}
}

func (c *helperTerminalConn) decodeScreenCheckpoint(frame protocol.TerminalOutputFrame) (helperOutput, error) {
	if len(frame.Data) == 0 || frame.Data[0] > 1 {
		return helperOutput{}, errors.New("invalid terminal screen checkpoint")
	}
	if !c.checkpointActive {
		c.checkpointActive = true
		c.checkpointSequence = frame.StartSequence
		c.checkpointBytes = 0
	}
	if frame.StartSequence != c.checkpointSequence || len(frame.Data)-1 > protocol.MaxTerminalScreenCheckpointBytes-c.checkpointBytes {
		return helperOutput{}, errors.New("terminal screen checkpoint exceeds its boundary or size")
	}
	c.checkpointBytes += len(frame.Data) - 1
	output := helperOutput{data: frame.Data[1:]}
	if frame.Data[0] == 1 {
		c.checkpointActive = false
		output.endSequence = frame.StartSequence
	}
	return output, nil
}

func helperResponseTerminalModes(frame helperFrame) string {
	var response struct {
		Result struct {
			Session struct {
				Snapshot struct {
					TerminalModes struct {
						AlternateScreen bool `json:"alternate_screen"`
						MouseClick      bool `json:"mouse_click"`
						MouseDrag       bool `json:"mouse_drag"`
						MouseMotion     bool `json:"mouse_motion"`
						MouseURXVT      bool `json:"mouse_urxvt"`
						MouseSGR        bool `json:"mouse_sgr"`
						FocusEvents     bool `json:"focus_events"`
						BracketedPaste  bool `json:"bracketed_paste"`
					} `json:"terminal_modes"`
				} `json:"snapshot"`
			} `json:"session"`
		} `json:"result"`
	}
	if json.Unmarshal(frame.Payload, &response) != nil {
		return ""
	}
	m := response.Result.Session.Snapshot.TerminalModes
	var out strings.Builder
	for _, item := range []struct {
		on bool
		id string
	}{{m.AlternateScreen, "1049"}, {m.MouseClick, "1000"}, {m.MouseDrag, "1002"}, {m.MouseMotion, "1003"}, {m.MouseURXVT, "1015"}, {m.MouseSGR, "1006"}, {m.BracketedPaste, "2004"}, {m.FocusEvents, "1004"}} {
		if item.on {
			out.WriteString("\x1b[?")
			out.WriteString(item.id)
			out.WriteByte('h')
		}
	}
	return out.String()
}

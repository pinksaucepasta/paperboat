package tunnel

import (
	"bytes"
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// Initial output and the attachment response use independent QUIC streams.
// Retain bounded, valid output until the response supplies its exact stream
// binding; each application's normal reader then validates and consumes it.
type initialHelperOutput struct {
	frames           [][]byte
	encoded, decoded int
}

func (q *initialHelperOutput) add(data []byte) error {
	info, err := protocol.InspectTerminalOutput(data)
	decoded := int(info.UncompressedLength)
	if err != nil || len(q.frames) >= 64 ||
		q.encoded > 4*helperMaxFrame-len(data) || q.decoded > 4*helperMaxFrame-decoded {
		return errors.New("helper sent excessive output before attachment")
	}
	q.frames = append(q.frames, bytes.Clone(data))
	q.encoded += len(data)
	q.decoded += decoded
	return nil
}

func awaitHelperResponse(ctx context.Context, message helperMessageConnection, requestID string, initial *initialHelperOutput) (helperFrame, error) {
	for {
		kind, data, err := message.ReadMessage(ctx)
		if err != nil {
			return helperFrame{}, err
		}
		if kind == helperBinaryMessage && initial != nil {
			if err := initial.add(data); err != nil {
				return helperFrame{}, err
			}
			continue
		}
		if kind != helperStructuredMessage {
			return helperFrame{}, errors.New("helper returned an unsupported message before attachment")
		}
		response, err := decodeHelperFrame(data)
		if err != nil {
			return helperFrame{}, err
		}
		if response.Type == "heartbeat" {
			if err := writeHelperFrame(ctx, message, response); err != nil {
				return helperFrame{}, err
			}
			continue
		}
		if response.RequestID != requestID {
			return helperFrame{}, errors.New("helper response did not match request")
		}
		return response, nil
	}
}

type initialOutputConnection struct {
	helperMessageConnection
	frames [][]byte
}

func (c *initialOutputConnection) ReadMessage(ctx context.Context) (helperMessageType, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if len(c.frames) == 0 {
		return c.helperMessageConnection.ReadMessage(ctx)
	}
	data := c.frames[0]
	c.frames[0] = nil
	c.frames = c.frames[1:]
	return helperBinaryMessage, data, nil
}

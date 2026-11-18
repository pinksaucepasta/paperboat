package execprocess

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
)

// A remote exec message is a length-prefixed frame: a 4-byte big-endian header length, the JSON header, then the raw
// payload (output bytes or stdin bytes). Bytes cross the session owner, hostd and worker hops as they are, never as
// base64 inside JSON (which cost two string scans and a base64 pass per hop and capped exec output near 17 MB/s).
// Terminal-output limits and the per-event acknowledgement are unchanged: this only replaces the encoding.
//
// MaxMessageBytes bounds one message, header and payload, and is the owner bridge's body limit. The header may use all
// of it, so the cap can never refuse a message the bridge would have taken. What fills a header is bounded below it:
// a list answer is paged (listPageSnapshots of ~0.5 KiB), a start is the validated request (arguments and environment
// <= 64 KiB each) plus the managed environment (<= 320 KiB), at worst 6x larger when every byte is a JSON-escaped control
// byte: ~2.7 MiB. Decoding refuses a header longer than the cap or than the frame (ErrInvalid: that is corruption);
// encoding refuses an oversized message (ErrMessageTooLarge).
const MaxMessageBytes = 8 << 20

// EncodeMessage frames header (as JSON) and payload.
func EncodeMessage(header any, payload []byte) ([]byte, error) {
	head, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if 4+len(head)+len(payload) > MaxMessageBytes {
		return nil, ErrMessageTooLarge
	}
	frame := make([]byte, 4, 4+len(head)+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(head)))
	frame = append(frame, head...)
	return append(frame, payload...), nil
}

// DecodeMessage unmarshals the header into header and returns the payload, which aliases frame.
func DecodeMessage(frame []byte, header any) ([]byte, error) {
	return decodeMessage(frame, header, false)
}

func decodeMessage(frame []byte, header any, strict bool) ([]byte, error) {
	if len(frame) < 4 {
		return nil, ErrInvalid
	}
	size := binary.BigEndian.Uint32(frame)
	if size > MaxMessageBytes || uint64(size) > uint64(len(frame)-4) {
		return nil, ErrInvalid
	}
	head := frame[4 : 4+size]
	if strict {
		decoder := json.NewDecoder(bytes.NewReader(head))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(header); err != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, ErrInvalid
		}
	} else if err := json.Unmarshal(head, header); err != nil {
		return nil, ErrInvalid
	}
	return frame[4+size:], nil
}

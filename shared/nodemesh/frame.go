package nodemesh

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/shared/strictjson"
)

// maxJSONDepth bounds nesting in a link message; every message here is flat.
const maxJSONDepth = 4

// WriteMessage writes v as one message: a 4-byte big-endian length, then JSON, at most MaxFrame bytes.
func WriteMessage(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxFrame {
		return ErrFrame
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[4:], body)
	_, err = w.Write(buf)
	return err
}

// ReadMessage reads one message into v. Unknown fields are ignored (a node one release ahead keeps
// working); the types it does read are checked by the message's Validate, which the caller runs.
func ReadMessage(r io.Reader, v any) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrame {
		return ErrFrame
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	if err := strictjson.DecodeResponse(body, v, maxJSONDepth); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

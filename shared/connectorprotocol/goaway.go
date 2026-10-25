package connectorprotocol

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"time"
)

// Goaway is the stream kind one side of a data carrier opens to say it is draining: the receiver
// opens no new streams on that carrier and lets the open ones finish until the deadline. The
// edge sends GoawayEdgeDraining, the host GoawayHostDraining. The stream carries one notice,
// length-prefixed like the stream-open preface, and the receiver closes the stream once it has it.
// Prior art: HTTP/2 GOAWAY; cloudflared UnregisterConnection then a grace period
// (connection/control.go).
const (
	Goaway             = "goaway"
	GoawayEdgeDraining = "edge_draining"
	GoawayHostDraining = "host_draining"
	maxGoawayBytes     = 1 << 10
)

// GoawayNotice is the body of a goaway stream.
type GoawayNotice struct {
	Reason        string `json:"reason"`
	DrainDeadline int64  `json:"drain_deadline"`
}

func (n GoawayNotice) validate() error {
	if n.Reason != GoawayEdgeDraining && n.Reason != GoawayHostDraining || n.DrainDeadline <= 0 {
		return ErrInvalidInput
	}
	return nil
}

// WriteGoawayNotice writes one notice draining until deadline.
func WriteGoawayNotice(writer io.Writer, reason string, deadline time.Time) error {
	notice := GoawayNotice{Reason: reason, DrainDeadline: deadline.Unix()}
	if writer == nil || notice.validate() != nil {
		return ErrInvalidInput
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		return CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	frame := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(payload)), uint32(len(payload)))
	if _, err := writer.Write(append(frame, payload...)); err != nil {
		return CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	return nil
}

// ReadGoawayNotice reads the one notice of a goaway stream.
func ReadGoawayNotice(reader io.Reader) (GoawayNotice, error) {
	if reader == nil {
		return GoawayNotice{}, ErrInvalidInput
	}
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return GoawayNotice{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxGoawayBytes {
		return GoawayNotice{}, ErrFrameTooLarge
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return GoawayNotice{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	var notice GoawayNotice
	if err := decodeStrict(payload, &notice); err != nil {
		return GoawayNotice{}, CodeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if err := notice.validate(); err != nil {
		return GoawayNotice{}, err
	}
	return notice, nil
}

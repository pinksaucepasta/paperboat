// Package wirejson decodes documents the control plane sends to this client.
//
// A server may add fields to a response at any time, and clients of every older version must keep
// working, so response decoding ignores unknown fields. What a client depends on is still checked:
// field types, required values and the document's schema and kind are validated by the caller, and
// trailing data after the document is rejected. Requests this client sends stay exact.
package wirejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// ErrTrailingData means the document is followed by more JSON.
var ErrTrailingData = errors.New("trailing data after JSON document")

// Decode decodes one JSON document into out, ignoring unknown fields.
func Decode(raw []byte, out any) error {
	return DecodeReader(bytes.NewReader(raw), out)
}

// DecodeReader decodes one JSON document from r into out, ignoring unknown fields.
func DecodeReader(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrTrailingData
	}
	return nil
}

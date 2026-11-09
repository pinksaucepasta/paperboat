// Package strictjson owns bounded closed-world JSON decoding for tunnel
// security and control boundaries.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var ErrInvalid = errors.New("invalid structured JSON")

func Decode(data []byte, target any, maximumDepth int) error {
	if target == nil {
		return ErrInvalid
	}
	if err := Validate(data, maximumDepth); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

// DecodeResponse decodes a server response: bounded depth, one value, typed known fields, and unknown
// fields ignored, so a field the server adds never takes down a running edge (AGENTS.md: clients ignore
// unknown fields). Signed authorization claims stay on Decode.
func DecodeResponse(data []byte, target any, maximumDepth int) error {
	if target == nil {
		return ErrInvalid
	}
	if err := Validate(data, maximumDepth); err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

func Validate(data []byte, maximumDepth int) error {
	if len(data) == 0 || maximumDepth < 1 {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeValue(decoder, 0, maximumDepth); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalid
	}
	return nil
}

func consumeValue(decoder *json.Decoder, depth, maximumDepth int) error {
	if depth > maximumDepth {
		return ErrInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalid
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrInvalid
			}
			seen[key] = struct{}{}
			if err := consumeValue(decoder, depth+1, maximumDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return ErrInvalid
		}
	case '[':
		for decoder.More() {
			if err := consumeValue(decoder, depth+1, maximumDepth); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// Package connectorprotocol implements the Paperboat connector control-plane
// protocol. It contains only control messages and session state. Byte transport,
// route forwarding, and origin proxying belong to their owning packages.
//
// Verify changes with this package's protocol tests and the affected peer
// implementations. No workspace fixture synchronization step is required.
package connectorprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
)

type Clock interface{ Now() time.Time }
type ClockFunc func() time.Time

func (f ClockFunc) Now() time.Time { return f() }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// PreparedConfig is a two-phase configuration change. Prepare must stage the
// candidate without changing active traffic. Activate is called only after
// edge, route, and origin readiness have all been observed. Abort is safe to
// call after any failed preparation or activation.
type PreparedConfig interface {
	Activate(context.Context) error
	Abort(context.Context) error
}

type ConfigApplier interface {
	PrepareSnapshot(context.Context, Snapshot) (PreparedConfig, error)
	PrepareDelta(context.Context, Delta) (PreparedConfig, error)
}

// canonicalJSON validates duplicate keys, depth, trailing data and forbidden
// secret-bearing fields before a payload is hashed or handed to an applier.
func canonicalJSON(data []byte, limit int) ([]byte, error) {
	if len(data) == 0 || len(data) > limit || rejectDuplicateKeys(data) != nil {
		return nil, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeValue(decoder, 0)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrInvalidInput
	}
	if err := rejectSecretFields(value); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > limit {
		return nil, ErrInvalidInput
	}
	return encoded, nil
}

func decodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key := keyToken.(string)
				child, err := decodeValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			values := make([]any, 0)
			for decoder.More() {
				child, err := decodeValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				values = append(values, child)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return values, nil
		default:
			return nil, errors.New("invalid delimiter")
		}
	default:
		return token, nil
	}
}

func rejectSecretFields(value any) error {
	var visit func(any) error
	visit = func(current any) error {
		switch value := current.(type) {
		case map[string]any:
			for key, child := range value {
				normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
				switch normalized {
				case "accesstoken", "apikey", "authorization", "clientsecret", "cookie", "headers", "password", "privatekey", "refreshtoken", "requestbody", "requestheaders", "responsebody", "responseheaders", "secret", "sessiontoken", "setcookie", "token":
					return ErrInvalidInput
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		case string:
			upper := strings.ToUpper(strings.TrimSpace(value))
			if strings.HasPrefix(upper, "BEARER ") || strings.Contains(upper, "BEGIN PRIVATE KEY") {
				return ErrInvalidInput
			}
			if parsed, err := url.Parse(value); err == nil && parsed.IsAbs() {
				if parsed.User != nil {
					return ErrInvalidInput
				}
				for key := range parsed.Query() {
					normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
					switch normalized {
					case "accesstoken", "apikey", "authorization", "clientsecret", "password", "refreshtoken", "secret", "sessiontoken", "token":
						return ErrInvalidInput
					}
				}
			}
		}
		return nil
	}
	return visit(value)
}

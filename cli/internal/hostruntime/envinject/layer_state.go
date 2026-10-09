package envinject

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumLayerStateBytes = 8 << 20

var ErrInvalidSnapshot = errors.New("invalid encrypted environment state")
var ErrObservationLost = errors.New("environment layer floor is unavailable")
var ErrResourceExhausted = errors.New("encrypted environment rollback protection reached its storage limit; rotate the affected scope keys or remove unused scopes before retrying")
var ErrNotReady = errors.New("encrypted environment is not ready")

type EnvironmentSource interface{ Environment() ([]string, error) }
type authenticatedLayerState struct {
	Record json.RawMessage `json:"record"`
	MAC    string          `json:"mac"`
}

func writeLayerState(path string, key []byte, value any) error {
	record, err := json.Marshal(value)
	if len(record) > maximumLayerStateBytes {
		return ErrResourceExhausted
	}
	if err != nil || len(key) != 32 {
		return ErrInvalidSnapshot
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("paperboat.environment.layer-state/v1\x00"))
	mac.Write(record)
	raw, err := json.Marshal(authenticatedLayerState{Record: record, MAC: base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicfile.Write(path, raw, atomicfile.CurrentOwnerOptions(0600))
}
func readLayerState(path string, key []byte, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !secureStateFile(path, info, maximumLayerStateBytes+256) {
		return ErrInvalidSnapshot
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if rejectDuplicateJSON(raw) != nil {
		return ErrInvalidSnapshot
	}
	var envelope authenticatedLayerState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&envelope) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return ErrInvalidSnapshot
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("paperboat.environment.layer-state/v1\x00"))
	mac.Write(envelope.Record)
	actual, err := base64.RawURLEncoding.Strict().DecodeString(envelope.MAC)
	if err != nil || !hmac.Equal(mac.Sum(nil), actual) {
		return ErrInvalidSnapshot
	}
	dec = json.NewDecoder(bytes.NewReader(envelope.Record))
	dec.DisallowUnknownFields()
	if dec.Decode(value) != nil || dec.Decode(&struct{}{}) != io.EOF {
		return ErrInvalidSnapshot
	}
	return nil
}
func validIdentifier(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func rejectDuplicateJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInvalidSnapshot
				}
				if _, duplicate := seen[key]; duplicate {
					return ErrInvalidSnapshot
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return ErrInvalidSnapshot
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidSnapshot
	}
	return nil
}

package envinject

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const maximumLayerStateBytes = 8 << 20

var ErrInvalidSnapshot = errors.New("invalid encrypted environment state")
var ErrObservationLost = errors.New("environment layer floor is unavailable")
var ErrResourceExhausted = errors.New("encrypted environment rollback protection reached its storage limit; rotate the affected scope keys or remove unused scopes before retrying")
var ErrNotReady = errors.New("encrypted environment is not ready")

// DeliveryError keeps ENV launch failures specific without exposing their cause.
type DeliveryError struct{ Cause error }

func (e *DeliveryError) Error() string {
	return "Managed environment variables could not be delivered. Check ENV delivery and retry."
}
func (e *DeliveryError) Unwrap() error { return e.Cause }

type layerStateFailure struct {
	message string
	cause   error
}

func (e *layerStateFailure) Error() string { return e.message }
func (e *layerStateFailure) Unwrap() error { return e.cause }

func safeLayerStateFailure(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return &layerStateFailure{message: message, cause: cause}
}

func invalidLayerStateFailure(cause error) error {
	if cause == nil {
		return ErrInvalidSnapshot
	}
	return safeLayerStateFailure("encrypted environment state is invalid", errors.Join(ErrInvalidSnapshot, cause))
}

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
		return safeLayerStateFailure("encrypted environment state could not be encoded", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return safeLayerStateFailure("encrypted environment state could not be prepared", err)
	}
	if err := atomicfile.Write(path, raw, atomicfile.CurrentOwnerOptions(0600)); err != nil {
		return safeLayerStateFailure("encrypted environment state could not be stored", err)
	}
	return nil
}
func readLayerState(path string, key []byte, value any) (missing bool, resultErr error) {
	info, err := os.Lstat(path)
	if err != nil {
		return os.IsNotExist(err), safeLayerStateFailure("encrypted environment state could not be inspected", err)
	}
	secure, err := secureStateFile(path, info, maximumLayerStateBytes+256)
	if err != nil {
		return false, safeLayerStateFailure("encrypted environment state security could not be checked", err)
	}
	if !secure {
		return false, ErrInvalidSnapshot
	}
	file, err := os.Open(path)
	if err != nil {
		return false, safeLayerStateFailure("encrypted environment state could not be read", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, int64(maximumLayerStateBytes+256+1)))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return false, safeLayerStateFailure("encrypted environment state could not be read", errors.Join(readErr, closeErr))
	}
	if len(raw) > maximumLayerStateBytes+256 {
		return false, ErrInvalidSnapshot
	}
	if err := rejectDuplicateJSON(raw); err != nil {
		if err == ErrInvalidSnapshot {
			return false, ErrInvalidSnapshot
		}
		return false, invalidLayerStateFailure(err)
	}
	var envelope authenticatedLayerState
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return false, invalidLayerStateFailure(err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return false, invalidLayerStateFailure(err)
		}
		return false, ErrInvalidSnapshot
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("paperboat.environment.layer-state/v1\x00"))
	mac.Write(envelope.Record)
	actual, err := base64.RawURLEncoding.Strict().DecodeString(envelope.MAC)
	if err != nil || !hmac.Equal(mac.Sum(nil), actual) {
		return false, ErrInvalidSnapshot
	}
	dec = json.NewDecoder(bytes.NewReader(envelope.Record))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return false, invalidLayerStateFailure(err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err != nil {
			return false, invalidLayerStateFailure(err)
		}
		return false, ErrInvalidSnapshot
	}
	return false, nil
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
	if _, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return ErrInvalidSnapshot
	}
	return nil
}

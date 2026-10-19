package preview

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

func decodeAttachmentEnvelope(data []byte, now time.Time) (Attachment, error) {
	if len(data) == 0 || len(data) > attachmentResponseMaxBytes {
		return Attachment{}, ErrAttachmentClientInvalid
	}
	if err := rejectAttachmentDuplicateFields(data); err != nil {
		return Attachment{}, fmt.Errorf("%w: response JSON: %v", ErrAttachmentClientInvalid, err)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Data) == 0 {
		return Attachment{}, fmt.Errorf("%w: invalid response envelope", ErrAttachmentClientInvalid)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Attachment{}, fmt.Errorf("%w: response has trailing data", ErrAttachmentClientInvalid)
	}
	if err := rejectAttachmentDuplicateFields(envelope.Data); err != nil {
		return Attachment{}, fmt.Errorf("%w: attachment JSON: %v", ErrAttachmentClientInvalid, err)
	}
	var attachment Attachment
	attachmentDecoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	attachmentDecoder.DisallowUnknownFields()
	if err := attachmentDecoder.Decode(&attachment); err != nil {
		return Attachment{}, fmt.Errorf("%w: invalid attachment", ErrAttachmentClientInvalid)
	}
	if err := attachmentDecoder.Decode(&struct{}{}); err != io.EOF {
		return Attachment{}, fmt.Errorf("%w: attachment has trailing data", ErrAttachmentClientInvalid)
	}
	if err := attachment.Validate(now); err != nil {
		return Attachment{}, err
	}
	return attachment, nil
}

func validateAttachmentForRequest(attachment Attachment, request AttachmentRequest) error {
	if attachment.PreviewID != request.PreviewID || attachment.OperationID != request.OperationID || attachment.OwnerMachineID != request.OwnerMachineID || attachment.OwnerSessionID != request.OwnerSessionID || attachment.IdempotencyKey != request.IdempotencyKey || attachment.RequestID != request.RequestID || attachment.CorrelationID != request.CorrelationID {
		return fmt.Errorf("%w: response identity differs from request", ErrAttachmentBinding)
	}
	// The authenticated lease revision can advance on a deadline-only renewal
	// while the server retains the exact admitted carrier generation.
	leaseGeneration := leaseGenerationForID(request.PreviewID, request.LeaseETag)
	if attachment.Binding.PreviewID != request.PreviewID || attachment.Binding.OperationID != request.OperationID || attachment.Binding.OwnerMachineID != request.OwnerMachineID || attachment.Binding.OwnerSessionID != request.OwnerSessionID || leaseGeneration < 1 || attachment.Binding.LeaseGeneration == 0 || attachment.Binding.LeaseGeneration > uint64(leaseGeneration) {
		return fmt.Errorf("%w: response binding differs from request", ErrAttachmentBinding)
	}
	want, err := request.Hash(attachment.AccountID)
	if err != nil || attachment.RequestHash != want {
		return fmt.Errorf("%w: response request hash differs from request", ErrAttachmentBinding)
	}
	return nil
}

func validateAttachmentTarget(target LeaseTarget) error {
	if len(target.Scheme) == 0 || len(target.Scheme) > 32 || len(target.Address) == 0 || len(target.Address) > 512 || hasAttachmentControl(target.Scheme) || hasAttachmentControl(target.Address) {
		return fmt.Errorf("%w: invalid attachment target", ErrAttachmentBinding)
	}
	switch strings.ToLower(target.Scheme) {
	case "http", "https", "h2c", "tcp", "unix":
		return nil
	default:
		return fmt.Errorf("%w: unsupported attachment target scheme", ErrAttachmentBinding)
	}
}

func validateAttachmentEndpoint(value string, edge bool) error {
	if len(value) == 0 || len(value) > 2048 || hasAttachmentControl(value) {
		return fmt.Errorf("%w: invalid attachment endpoint", ErrAttachmentBinding)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: endpoint must be an absolute URL without credentials or query", ErrAttachmentBinding)
	}
	if edge {
		switch strings.ToLower(parsed.Scheme) {
		case "h2", "h3":
		default:
			return fmt.Errorf("%w: unsupported edge endpoint scheme", ErrAttachmentBinding)
		}
	} else if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("%w: endpoint must use https", ErrAttachmentBinding)
	}
	return nil
}

func validAttachmentContentHash(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// validMachineIdentityPublicKey and machineIdentityThumbprint mirror the
// previewattachment server contract. The binding uses the raw Ed25519 public
// key hash (rather than the local identity store's RFC 7638 key thumbprint)
// because the edge uses the same bytes to bind the mTLS peer.
func validMachineIdentityPublicKey(value string) bool {
	if value == "" {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == ed25519.PublicKeySize && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validMachineIdentityThumbprint(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*4/3+1 {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == strings.TrimPrefix(value, prefix)
}

func machineIdentityThumbprint(publicKey string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return ""
	}
	digest := sha256.Sum256(decoded)
	return "sha256:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func validAttachmentID(value string) bool {
	if len(value) < 3 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e || strings.ContainsRune("/?#\\", char) {
			return false
		}
	}
	return true
}

func hasAttachmentControl(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

func rejectAttachmentDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, isDelimiter := token.(json.Delim)
		if !isDelimiter {
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
					return ErrAttachmentClientInvalid
				}
				if _, exists := seen[key]; exists {
					return errors.New("duplicate field")
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
			return ErrAttachmentClientInvalid
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrAttachmentClientInvalid
	}
	return nil
}

var _ interface {
	Allocate(context.Context, AttachmentRequest) (Attachment, error)
} = (*AttachmentClient)(nil)

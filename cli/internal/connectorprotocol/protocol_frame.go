package connectorprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type MessageType string

const (
	MessageHello                       MessageType = "hello"
	MessageWelcome                     MessageType = "welcome"
	MessageSnapshot                    MessageType = "snapshot"
	MessageDelta                       MessageType = "delta"
	MessageAck                         MessageType = "ack"
	MessageReady                       MessageType = "ready"
	MessageHeartbeat                   MessageType = "heartbeat"
	MessageHeartbeatAck                MessageType = "heartbeat_ack"
	MessageAuthRenew                   MessageType = "auth_renew"
	MessageAuthRenewed                 MessageType = "auth_renewed"
	MessageDrain                       MessageType = "drain"
	MessageDrainAck                    MessageType = "drain_ack"
	MessageCredentialRotationChallenge MessageType = "credential_rotation_challenge"
	MessageCredentialRotationProof     MessageType = "credential_rotation_proof"
	MessageCredentialRotationInstall   MessageType = "credential_rotation_install"
	MessageCredentialRotationReady     MessageType = "credential_rotation_ready"
	MessageCredentialRotationRevoke    MessageType = "credential_rotation_revoke"
	MessageCredentialRotationAck       MessageType = "credential_rotation_ack"
	MessageDisconnect                  MessageType = "disconnect"
	MessageReject                      MessageType = "reject"
)

const (
	MessageRotationChallenge = MessageCredentialRotationChallenge
	MessageRotationProof     = MessageCredentialRotationProof
	MessageRotationInstall   = MessageCredentialRotationInstall
	MessageRotationReady     = MessageCredentialRotationReady
	MessageRotationRevoke    = MessageCredentialRotationRevoke
	MessageRotationAck       = MessageCredentialRotationAck
)

var allowedMessageTypes = map[MessageType]struct{}{
	MessageHello: {}, MessageWelcome: {}, MessageSnapshot: {}, MessageDelta: {},
	MessageAck: {}, MessageReady: {}, MessageHeartbeat: {}, MessageHeartbeatAck: {},
	MessageAuthRenew: {}, MessageAuthRenewed: {}, MessageDrain: {}, MessageDrainAck: {},
	MessageCredentialRotationChallenge: {}, MessageCredentialRotationProof: {}, MessageCredentialRotationInstall: {}, MessageCredentialRotationReady: {}, MessageCredentialRotationRevoke: {}, MessageCredentialRotationAck: {},
	MessageDisconnect: {}, MessageReject: {},
}

type Frame struct {
	Type      MessageType     `json:"type"`
	Version   string          `json:"version"`
	RequestID string          `json:"request_id"`
	Payload   json.RawMessage `json:"payload"`
}

func NewFrame(messageType MessageType, requestID string, payload any) (Frame, error) {
	if _, ok := allowedMessageTypes[messageType]; !ok || !validRequestID(requestID) {
		return Frame{}, ErrInvalidInput
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Frame{}, codeError(ErrInvalidInput, ReasonMalformed, false, err)
	}
	if payloadTooLarge(messageType, payload) {
		return Frame{}, ErrFrameTooLarge
	}
	if len(encoded) > maxPayloadBytes(messageType) {
		return Frame{}, ErrFrameTooLarge
	}
	if err := validatePayloadJSON(encoded); err != nil {
		return Frame{}, err
	}
	if err := validateMessagePayload(messageType, encoded); err != nil {
		return Frame{}, err
	}
	return Frame{Type: messageType, Version: ProtocolVersion, RequestID: requestID, Payload: encoded}, nil
}

func payloadTooLarge(messageType MessageType, payload any) bool {
	switch messageType {
	case MessageSnapshot:
		if value, ok := payload.(Snapshot); ok {
			return len(value.Payload) > MaxSnapshotBytes
		}
	case MessageDelta:
		if value, ok := payload.(Delta); ok {
			return len(value.Payload) > MaxDeltaBytes
		}
	}
	return false
}

func (f Frame) Validate() error {
	if _, ok := allowedMessageTypes[f.Type]; !ok {
		return codeError(ErrUnsupportedMessage, ReasonMalformed, false, nil)
	}
	if f.Version != ProtocolVersion || !validRequestID(f.RequestID) || len(f.Payload) == 0 || len(f.Payload) > maxPayloadBytes(f.Type) {
		return ErrMalformedFrame
	}
	if err := validatePayloadJSON(f.Payload); err != nil {
		return err
	}
	return validateMessagePayload(f.Type, f.Payload)
}

func validateMessagePayload(messageType MessageType, payload []byte) error {
	switch messageType {
	case MessageHello:
		var message Hello
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageWelcome:
		var message Welcome
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageSnapshot:
		var message Snapshot
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageDelta:
		var message Delta
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageAck:
		var message Ack
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageReady:
		var message Readiness
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageHeartbeat:
		var message Heartbeat
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageHeartbeatAck:
		var message HeartbeatAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageAuthRenew:
		var message RenewalRequest
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageAuthRenewed:
		var message AuthResult
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.ValidateBound()
	case MessageDrain:
		var message Drain
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageDrainAck:
		var message DrainAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationChallenge:
		var message CredentialRotationChallenge
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationProof:
		var message CredentialRotationProof
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationInstall:
		var message CredentialRotationInstall
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationReady:
		var message CredentialRotationReady
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationRevoke:
		var message CredentialRotationRevoke
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate(time.Time{})
	case MessageCredentialRotationAck:
		var message CredentialRotationAck
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageDisconnect:
		var message Disconnect
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	case MessageReject:
		var message Reject
		if err := decodeStrict(payload, &message); err != nil {
			return ErrMalformedFrame
		}
		return message.Validate()
	}
	return ErrUnsupportedMessage
}

func (f Frame) DecodePayload(target any) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return decodeStrict(f.Payload, target)
}

func ReadFrame(reader io.Reader) (Frame, error) {
	if reader == nil {
		return Frame{}, ErrInvalidInput
	}
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return Frame{}, codeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length == 0 {
		return Frame{}, ErrMalformedFrame
	}
	if length > MaxFrameBytes {
		return Frame{}, ErrFrameTooLarge
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(reader, body); err != nil {
		return Frame{}, codeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if !json.Valid(body) || rejectDuplicateKeys(body) != nil {
		return Frame{}, ErrMalformedFrame
	}
	var frame Frame
	if err := decodeStrict(body, &frame); err != nil {
		return Frame{}, codeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if err := frame.Validate(); err != nil {
		return Frame{}, err
	}
	return frame, nil
}

func WriteFrame(writer io.Writer, frame Frame) error {
	if writer == nil {
		return ErrInvalidInput
	}
	if err := frame.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(frame)
	if err != nil {
		return codeError(ErrMalformedFrame, ReasonMalformed, false, err)
	}
	if len(body) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return err
	}
	return writeAll(writer, body)
}

func maxPayloadBytes(messageType MessageType) int {
	switch messageType {
	case MessageSnapshot:
		return MaxSnapshotBytes + 16<<10
	case MessageDelta:
		return MaxDeltaBytes + 16<<10
	default:
		return 64 << 10
	}
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func validRequestID(value string) bool {
	return len(value) >= 1 && len(value) <= MaxIdentifierBytes && identifierPattern.MatchString(value)
}

func decodeStrict(data []byte, target any) error {
	if len(data) == 0 || len(data) > MaxFrameBytes || target == nil || rejectDuplicateKeys(data) != nil {
		return ErrMalformedFrame
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validatePayloadJSON(data []byte) error {
	if len(data) == 0 || len(data) > MaxFrameBytes || !json.Valid(data) {
		return ErrMalformedFrame
	}
	if rejectDuplicateKeys(data) != nil {
		return ErrMalformedFrame
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > MaxJSONDepth {
			return errors.New("JSON nesting exceeds limit")
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
					return errors.New("object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return errors.New("duplicate object key")
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

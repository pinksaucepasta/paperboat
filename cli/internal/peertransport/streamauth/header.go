// Package streamauth owns the canonical authorization header for one
// operation opened over a reusable peer transport.
package streamauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/peerconsumer"
	"strings"
	"time"
)

const (
	Version           = 1
	MaximumHeaderSize = 24 << 10
	MaximumCredential = 16 << 10
	MaximumTarget     = 4 << 10
)

var ErrInvalid = errors.New("invalid peer stream authorization header")

// ErrExpired is a well-formed header whose deadline (the credential's server-clock expiry) has passed.
var ErrExpired = errors.New("peer stream authorization deadline passed")

type Header struct {
	Version      int    `json:"version"`
	OperationID  string `json:"operation_id"`
	Consumer     string `json:"consumer"`
	StreamID     string `json:"stream_id"`
	Credential   string `json:"credential"`
	DeadlineUnix int64  `json:"deadline_unix"`
	MaximumBytes uint64 `json:"maximum_bytes"`
	Resumable    bool   `json:"resumable"`
	// Target is a canonical server-owned binding for native private streams.
	// It is empty for every other consumer and is revalidated with Credential
	// by the receiving host before application dispatch.
	Target         string `json:"target,omitempty"`
	UsageSessionID string `json:"usage_session_id,omitempty"`
	// Offline asks the host to admit a new session from the signed session scopes it holds,
	// because the client could not reach Paperboat (F21-11). It carries no credential: the
	// native session already proves the peer, and the host decides from its own state.
	Offline bool `json:"offline,omitempty"`
	// Resume names the access session a terminal attachment was admitted under when it moves to a
	// new carrier without a fresh credential. It carries no secret: the host checks the peer's current
	// authority here and the attachment's resume secret inside the stream.
	Resume string `json:"resume,omitempty"`
	// LoginUser is the OS user an ssh stream logs in as (OpenSSH %r). The host checks it against the machine's Remote
	// Login before sshd does; empty means unknown and the host leaves the decision to sshd.
	LoginUser string `json:"login_user,omitempty"`
}

func NewOffline(operationID, consumer, streamID string, deadline time.Time, maximumBytes uint64) (Header, error) {
	value := Header{Version: Version, OperationID: operationID, Consumer: consumer, StreamID: streamID, DeadlineUnix: deadline.UTC().Unix(), MaximumBytes: maximumBytes, Offline: true}
	if value.Validate(time.Time{}) != nil {
		return Header{}, ErrInvalid
	}
	return value, nil
}

// NewResume opens a stream that moves a terminal attachment of sessionID; see Header.Resume.
func NewResume(sessionID, streamID, resource string, deadline time.Time, maximumBytes uint64) (Header, error) {
	value := Header{Version: Version, OperationID: sessionID, Consumer: "terminal", StreamID: streamID, DeadlineUnix: deadline.UTC().Unix(), MaximumBytes: maximumBytes, Resume: resource}
	if value.Validate(time.Time{}) != nil {
		return Header{}, ErrInvalid
	}
	return value, nil
}

func NewNativePrivate(operationID, consumer, streamID, credential string, deadline time.Time, maximumBytes uint64, target []byte) (Header, error) {
	value := Header{Version: Version, OperationID: operationID, Consumer: consumer, StreamID: streamID, Credential: credential, DeadlineUnix: deadline.UTC().Unix(), MaximumBytes: maximumBytes, Target: string(target)}
	if value.Validate(time.Time{}) != nil {
		return Header{}, ErrInvalid
	}
	return value, nil
}

func New(operationID, consumer, streamID, credential string, deadline time.Time, maximumBytes uint64) (Header, error) {
	value := Header{Version: Version, OperationID: operationID, Consumer: consumer, StreamID: streamID, Credential: credential, DeadlineUnix: deadline.UTC().Unix(), MaximumBytes: maximumBytes}
	if value.Validate(time.Time{}) != nil {
		return Header{}, ErrInvalid
	}
	return value, nil
}

func (h Header) Validate(now time.Time) error {
	if h.Version != Version || h.OperationID == "" || len(h.OperationID) > 128 || h.Consumer == "" || len(h.Consumer) > 128 || h.StreamID == "" || len(h.StreamID) > 128 || len(h.Credential) > MaximumCredential || h.DeadlineUnix <= 0 || h.MaximumBytes == 0 || len(h.Target) > MaximumTarget {
		return ErrInvalid
	}
	resume := h.Resume != ""
	if (h.Offline || resume) != (h.Credential == "") || h.Offline && !peerconsumer.Offline(h.Consumer) {
		return ErrInvalid
	}
	if resume && (h.Offline || h.Consumer != "terminal" || len(h.Resume) > 128) {
		return ErrInvalid
	}
	if len(h.UsageSessionID) > 128 || strings.ContainsAny(h.UsageSessionID, "\x00\r\n") || h.UsageSessionID != "" && h.Consumer != "config_compare" {
		return ErrInvalid
	}
	if h.LoginUser != "" && (h.Consumer != "ssh" || !ValidLoginUser(h.LoginUser)) {
		return ErrInvalid
	}
	private := h.Consumer == "private_http" || h.Consumer == "private_tcp" || h.Consumer == "inspector"
	if private != (h.Target != "") || h.Target != "" && !json.Valid([]byte(h.Target)) {
		return ErrInvalid
	}
	deadline := time.Unix(h.DeadlineUnix, 0)
	if !now.IsZero() && !deadline.After(now) {
		return ErrExpired
	}
	return nil
}

func (h Header) MarshalBinary() ([]byte, error) {
	if h.Validate(time.Time{}) != nil {
		return nil, ErrInvalid
	}
	encoded, err := json.Marshal(h)
	if err != nil || len(encoded) > MaximumHeaderSize {
		return nil, ErrInvalid
	}
	return encoded, nil
}

func Parse(encoded []byte, now time.Time) (Header, error) {
	if len(encoded) == 0 || len(encoded) > MaximumHeaderSize {
		return Header{}, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var value Header
	if err := decoder.Decode(&value); err != nil {
		return Header{}, ErrInvalid
	}
	if err := value.Validate(now); err != nil {
		return Header{}, err
	}
	var extra any
	if decoder.Decode(&extra) == nil {
		return Header{}, ErrInvalid
	}
	canonical, err := value.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, encoded) {
		return Header{}, ErrInvalid
	}
	return value, nil
}

// ValidLoginUser is a portable account name: what OpenSSH accepts after %r, never an option or a path.
func ValidLoginUser(user string) bool {
	if user == "" || len(user) > 64 || user[0] == '-' || user[0] == '.' {
		return false
	}
	for _, r := range user {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

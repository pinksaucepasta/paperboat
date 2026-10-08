//go:build js && wasm

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"syscall/js"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

const maximumCompareSideBytes = 100 << 20

type compareRequest struct {
	Action                 string `json:"action"`
	AssignmentID           string `json:"assignment_id"`
	AssignmentVersion      int64  `json:"assignment_version"`
	Path                   string `json:"path"`
	ConflictRevision       string `json:"conflict_revision"`
	ExpectedRemoteRevision string `json:"expected_remote_revision"`
}
type compareSide struct {
	Present bool   `json:"present"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}
type compareMetadata struct {
	compareRequest
	StreamID uint32      `json:"stream_id"`
	Local    compareSide `json:"local"`
	Managed  compareSide `json:"managed"`
}

func apiCompareConfig(_ js.Value, args []js.Value) any {
	if len(args) != 3 || args[1].Type() != js.TypeObject || args[2].Type() != js.TypeString {
		return errorObject("invalid_request", "Invalid config comparison request.")
	}
	config, err := parseBrowserTransportConfig(args[1], false)
	if err != nil {
		return errorObject("invalid_request", "Invalid config comparison transport.")
	}
	var request compareRequest
	if json.Unmarshal([]byte(args[2].String()), &request) != nil || request.Action != "compare" || request.AssignmentID == "" || request.AssignmentVersion < 1 || request.Path == "" || len(request.ConflictRevision) != 64 || len(request.ExpectedRemoteRevision) != 40 {
		return errorObject("invalid_request", "Invalid config comparison binding.")
	}
	identityID := args[0].String()
	browserRuntime.Lock()
	identity := browserRuntime.identities[identityID]
	delete(browserRuntime.identities, identityID)
	browserRuntime.Unlock()
	if identity == nil {
		return errorObject("identity_missing", "The temporary browser key expired. Retry the comparison.")
	}
	executor := js.FuncOf(func(_ js.Value, callbacks []js.Value) any {
		resolve := callbacks[0]
		go func() {
			defer zero(identity.PrivateKey)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			metadata, local, managed, err := readConfigComparison(ctx, identity, config, request)
			defer clear(local)
			defer clear(managed)
			if err != nil {
				resolve.Invoke(errorObject("comparison_failed", "The conflict comparison could not be verified. Refresh the conflict and retry."))
				return
			}
			raw, _ := json.Marshal(metadata)
			resolve.Invoke(jsObject(map[string]any{"metadata": string(raw), "local": jsByteArray(local), "managed": jsByteArray(managed)}))
		}()
		return nil
	})
	promise := js.Global().Get("Promise").New(executor)
	executor.Release()
	return promise
}
func readConfigComparison(ctx context.Context, identity *browserIdentity, config terminalConfig, request compareRequest) (compareMetadata, []byte, []byte, error) {
	metadata := compareMetadata{}
	ws, err := newBrowserWSConn(config.URL, config.Subprotocols)
	if err != nil {
		return metadata, nil, nil, err
	}
	defer ws.Close()
	if err = ws.waitOpen(ctx); err != nil {
		return metadata, nil, nil, err
	}
	initial, err := ws.readInitialMessage(ctx)
	if err != nil || !initial.binary {
		return metadata, nil, nil, errInvalidPeerIdentity
	}
	root, _ := base64.RawURLEncoding.Strict().DecodeString(config.RootPublicKey)
	peer, err := verifyIdentityEnvelope(initial.data, ed25519.PublicKey(root), config.RootKeyID, config.OwnerAccountID, config.MachineID, time.Now())
	if err != nil {
		return metadata, nil, nil, err
	}
	if ws.socket.Get("protocol").String() != "paperboat.browser-config-compare.e2ee.v1" {
		return metadata, nil, nil, errInvalidPeerIdentity
	}
	conn := tls.Client(ws, tlsClientConfig(identity, peer, time.Now))
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return metadata, nil, nil, err
	}
	if err = conn.HandshakeContext(ctx); err != nil {
		return metadata, nil, nil, err
	}
	hello := protocol.Frame{Type: "hello", RequestID: "comparison-hello", Version: protocol.ProtocolVersion, Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["config.compare.v1"]}`)}
	if err = writeStructuredFrame(conn, hello); err != nil {
		return metadata, nil, nil, err
	}
	welcome, err := readStructuredFrame(conn)
	if err != nil || welcome.Type != "welcome" {
		return metadata, nil, nil, errors.New("comparison handshake failed")
	}
	var negotiation struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(welcome.Payload, &negotiation) != nil || len(negotiation.Capabilities) != 1 || negotiation.Capabilities[0] != "config.compare.v1" {
		return metadata, nil, nil, errors.New("comparison capability unavailable")
	}
	payload, _ := json.Marshal(request)
	if err = writeStructuredFrame(conn, protocol.Frame{Type: "request", RequestID: "comparison-read", Version: protocol.ProtocolVersion, Capability: "config.compare.v1", Payload: payload}); err != nil {
		return metadata, nil, nil, err
	}
	response, err := readStructuredFrame(conn)
	if err != nil || response.Type != "response" || response.RequestID != "comparison-read" || json.Unmarshal(response.Payload, &metadata) != nil {
		return metadata, nil, nil, errors.New("comparison response invalid")
	}
	if metadata.AssignmentID != request.AssignmentID || metadata.AssignmentVersion != request.AssignmentVersion || metadata.Path != request.Path || metadata.ConflictRevision != request.ConflictRevision || metadata.ExpectedRemoteRevision != request.ExpectedRemoteRevision || metadata.StreamID == 0 {
		return metadata, nil, nil, errors.New("comparison identity mismatch")
	}
	for _, side := range []compareSide{metadata.Local, metadata.Managed} {
		if side.Bytes < 0 || side.Bytes > maximumCompareSideBytes || side.Present && (len(side.SHA256) != 64 || (side.Kind != "file" && side.Kind != "symlink")) || !side.Present && (side.Bytes != 0 || side.Kind != "deleted") {
			return metadata, nil, nil, errors.New("comparison side invalid")
		}
	}
	local := make([]byte, metadata.Local.Bytes)
	managed := make([]byte, metadata.Managed.Bytes)
	offsets := map[byte]uint64{1: 0, 2: 0}
	fail := func(err error) (compareMetadata, []byte, []byte, error) {
		clear(local)
		clear(managed)
		return compareMetadata{}, nil, nil, err
	}
	for {
		kind, payload, err := readApplicationFrame(conn)
		if err != nil {
			return fail(err)
		}
		if kind == appKindBinary {
			frame, err := protocol.DecodeTerminalOutput(payload)
			if err != nil || frame.StreamID != metadata.StreamID || frame.Channel < 1 || frame.Channel > 2 || len(frame.Data) == 0 {
				return fail(errors.New("comparison stream invalid"))
			}
			target := local
			if frame.Channel == 2 {
				target = managed
			}
			offset := offsets[frame.Channel]
			if frame.StartSequence != offset || offset+uint64(len(frame.Data)) > uint64(len(target)) {
				return fail(errors.New("comparison stream size mismatch"))
			}
			copy(target[offset:], frame.Data)
			offsets[frame.Channel] += uint64(len(frame.Data))
			continue
		}
		frame, err := decodeStructuredFrame(payload)
		if err != nil || frame.Type != "event" {
			return fail(errors.New("comparison stream incomplete"))
		}
		var end struct {
			Type     string `json:"type"`
			StreamID uint32 `json:"stream_id"`
		}
		if json.Unmarshal(frame.Payload, &end) != nil || end.Type != "comparison_complete" || end.StreamID != metadata.StreamID {
			return fail(errors.New("comparison completion invalid"))
		}
		if offsets[1] != uint64(len(local)) || offsets[2] != uint64(len(managed)) {
			return fail(errors.New("comparison truncated"))
		}
		for index, side := range []compareSide{metadata.Local, metadata.Managed} {
			if !side.Present {
				continue
			}
			value := local
			if index == 1 {
				value = managed
			}
			hash := sha256.New()
			if side.Kind == "symlink" {
				hash.Write([]byte("symlink:"))
			}
			hash.Write(value)
			if hex.EncodeToString(hash.Sum(nil)) != side.SHA256 {
				return fail(errors.New("comparison digest mismatch"))
			}
		}
		return metadata, local, managed, nil
	}
}

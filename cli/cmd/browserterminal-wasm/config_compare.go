package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"io"
	"slices"
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

func readConfigComparisonContent(conn io.ReadWriter, request compareRequest, operationID string) (compareMetadata, []byte, []byte, error) {
	metadata := compareMetadata{}
	var err error
	hello := protocol.Frame{Type: "hello", RequestID: "comparison-hello", Version: protocol.ProtocolVersion, Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1","config.compare.v1"]}`)}
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
	if json.Unmarshal(welcome.Payload, &negotiation) != nil || !slices.Contains(negotiation.Capabilities, "config.compare.v1") {
		return metadata, nil, nil, errors.New("comparison capability unavailable")
	}
	payload, _ := json.Marshal(request)
	if err = writeStructuredFrame(conn, protocol.Frame{Type: "request", RequestID: "comparison-read", Version: protocol.ProtocolVersion, Capability: "config.compare.v1", OperationID: operationID, DeadlineMS: 120000, Payload: payload}); err != nil {
		return metadata, nil, nil, err
	}
	response, err := readStructuredFrame(conn)
	var result struct {
		Result compareMetadata `json:"result"`
	}
	if err != nil || response.Type != "response" || response.RequestID != "comparison-read" || json.Unmarshal(response.Payload, &result) != nil {
		return metadata, nil, nil, errors.New("comparison response invalid")
	}
	metadata = result.Result
	if metadata.AssignmentID != request.AssignmentID || metadata.AssignmentVersion != request.AssignmentVersion || metadata.Path != request.Path || metadata.ConflictRevision != request.ConflictRevision || metadata.ExpectedRemoteRevision != request.ExpectedRemoteRevision || metadata.StreamID == 0 {
		return metadata, nil, nil, errors.New("comparison identity mismatch")
	}
	for _, side := range []compareSide{metadata.Local, metadata.Managed} {
		if side.Bytes < 0 || side.Bytes > maximumCompareSideBytes || side.Present && (len(side.SHA256) != 64 || (side.Kind != "regular" && side.Kind != "symlink")) || !side.Present && (side.Bytes != 0 || side.Kind != "deleted") {
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

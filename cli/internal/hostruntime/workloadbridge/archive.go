package workloadbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
)

// ArchiveMetadata contains stopped-session metadata, never executable input or
// terminal payloads. Available is false while an owner still holds its store.
type ArchiveMetadata struct {
	Available  bool               `json:"available"`
	Generation string             `json:"generation"`
	Snapshots  []session.Snapshot `json:"snapshots"`
}

func (c *Client) ReadArchiveMetadata(ctx context.Context, generation string) (ArchiveMetadata, error) {
	request, err := json.Marshal(struct {
		Generation string `json:"generation"`
	}{generation})
	if err != nil {
		return ArchiveMetadata{}, err
	}
	body, err := c.Call(ctx, "owner", "archive-metadata", request)
	if err != nil {
		return ArchiveMetadata{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var metadata ArchiveMetadata
	var extra any
	if decoder.Decode(&metadata) != nil || decoder.Decode(&extra) != io.EOF || metadata.Generation != generation || metadata.Available && metadata.Snapshots == nil {
		return ArchiveMetadata{}, session.ErrOwnerUnavailable
	}
	return metadata, nil
}

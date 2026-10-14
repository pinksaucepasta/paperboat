package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type VaultRecordState struct {
	WorkspaceID  string `json:"workspace_id"`
	OwnerKind    string `json:"owner_kind"`
	OwnerID      string `json:"owner_id"`
	MachineID    string `json:"machine_id"`
	KeyEpoch     uint64 `json:"key_epoch"`
	RecordID     string `json:"record_id"`
	Revision     uint64 `json:"revision"`
	Deleted      bool   `json:"deleted"`
	Sequence     uint64 `json:"sequence"`
	DocumentID   string `json:"document_id"`
	WriterPublic string `json:"writer_public"`
	Envelope     string `json:"envelope"`
}

func (s VaultRecordState) Decode() (env.VaultRecord, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s.Envelope)
	if err != nil {
		return env.VaultRecord{}, err
	}
	writer, err := base64.RawURLEncoding.Strict().DecodeString(s.WriterPublic)
	if err != nil {
		return env.VaultRecord{}, err
	}
	record, err := env.ParseVaultRecord(raw, writer)
	if err != nil {
		return env.VaultRecord{}, err
	}
	id, err := env.ParseDocumentID(s.DocumentID)
	c := record.Claims
	if err != nil || id != record.ID || c.WorkspaceID != s.WorkspaceID || c.OwnerKind != s.OwnerKind || c.OwnerID != s.OwnerID || c.MachineID != s.MachineID || c.KeyEpoch != s.KeyEpoch || c.RecordID != s.RecordID || c.Revision != s.Revision || c.Deleted != s.Deleted {
		return env.VaultRecord{}, errors.New("invalid encrypted ENV record")
	}
	return record, nil
}

type VaultRecordMutation struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	Envelope         string `json:"envelope"`
}
type VaultRecordsPut struct {
	OperationID string                `json:"operation_id"`
	Records     []VaultRecordMutation `json:"records"`
}
type VaultRecordsPage struct {
	Records       []VaultRecordState `json:"records"`
	Sequence      uint64             `json:"sequence"`
	AfterSequence uint64             `json:"after_sequence"`
	NextCursor    string             `json:"next_cursor"`
}
type VaultRecordsResult struct {
	Records  []VaultRecordState `json:"records"`
	Sequence uint64             `json:"sequence"`
}

func vaultRecordsPath(c VaultLayerCoordinate) string {
	return "/v1/environment/scopes/" + url.PathEscape(c.OwnerKind) + "/" + url.PathEscape(c.OwnerID) + "/records"
}
func (c *Client) GetVaultRecord(ctx context.Context, coordinate VaultLayerCoordinate, id string) (VaultRecordState, error) {
	scoped := *c
	if err := scoped.SetWorkspace(coordinate.WorkspaceID); err != nil {
		return VaultRecordState{}, err
	}
	q := url.Values{"machine_id": {coordinate.MachineID}}
	var out VaultRecordState
	err := scoped.vaultDataRequest(ctx, http.MethodGet, vaultRecordsPath(coordinate)+"/"+url.PathEscape(id)+"?"+q.Encode(), nil, &out)
	return out, err
}
func (c *Client) PutVaultRecords(ctx context.Context, coordinate VaultLayerCoordinate, in VaultRecordsPut) (VaultRecordsResult, error) {
	scoped := *c
	if err := scoped.SetWorkspace(coordinate.WorkspaceID); err != nil {
		return VaultRecordsResult{}, err
	}
	var out VaultRecordsResult
	q := url.Values{"machine_id": {coordinate.MachineID}}
	err := scoped.vaultDataRequest(ctx, http.MethodPut, vaultRecordsPath(coordinate)+"?"+q.Encode(), in, &out)
	return out, err
}
func (c *Client) VaultRecordsPage(ctx context.Context, coordinate VaultLayerCoordinate, after, through uint64, cursor string) (VaultRecordsPage, error) {
	scoped := *c
	if err := scoped.SetWorkspace(coordinate.WorkspaceID); err != nil {
		return VaultRecordsPage{}, err
	}
	q := url.Values{"machine_id": {coordinate.MachineID}, "after_sequence": {strconv.FormatUint(after, 10)}, "limit": {"200"}}
	if through > 0 || cursor != "" {
		q.Set("through_sequence", strconv.FormatUint(through, 10))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out VaultRecordsPage
	err := scoped.vaultDataRequest(ctx, http.MethodGet, vaultRecordsPath(coordinate)+"?"+q.Encode(), nil, &out)
	return out, err
}

// VaultRecords freezes one watermark and rejects repeated identities/cursors.
// Callers must apply the complete result atomically, including tombstones.
func (c *Client) VaultRecords(ctx context.Context, coordinate VaultLayerCoordinate, after uint64) (VaultRecordsResult, error) {
	var out VaultRecordsResult
	out.Records = []VaultRecordState{}
	cursor := ""
	seen := map[string]bool{}
	cursors := map[string]bool{}
	for page := 0; page < 32; page++ {
		next, err := c.VaultRecordsPage(ctx, coordinate, after, out.Sequence, cursor)
		if err != nil {
			return VaultRecordsResult{}, err
		}
		if page == 0 {
			out.Sequence = next.Sequence
		}
		if next.Sequence != out.Sequence || next.AfterSequence != after || next.Sequence < after || len(next.Records) > 200 {
			return VaultRecordsResult{}, errors.New("invalid ENV record pagination")
		}
		for _, r := range next.Records {
			if seen[r.RecordID] || r.WorkspaceID != coordinate.WorkspaceID || r.OwnerKind != coordinate.OwnerKind || r.OwnerID != coordinate.OwnerID || r.MachineID != coordinate.MachineID || r.Sequence > out.Sequence || after > 0 && r.Sequence <= after {
				return VaultRecordsResult{}, errors.New("inconsistent ENV record snapshot")
			}
			seen[r.RecordID] = true
			out.Records = append(out.Records, r)
		}
		if next.NextCursor == "" {
			return out, nil
		}
		if len(next.Records) == 0 || cursors[next.NextCursor] {
			return VaultRecordsResult{}, errors.New("ENV record cursor did not advance")
		}
		cursors[next.NextCursor] = true
		cursor = next.NextCursor
	}
	return VaultRecordsResult{}, errors.New("ENV record pagination exceeded scope bounds")
}

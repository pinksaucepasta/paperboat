package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/pinksaucepasta/paperboat/shared/recording"
)

var errInvalidReplay = errors.New("paperboat-server returned an invalid replay answer")

// ListRecordings is one page of a team's recordings this account may see (GET /v1/teams/{team}/recordings).
func (c *Client) ListRecordings(ctx context.Context, teamID, machineID string, limit, offset int) (recording.List, error) {
	query := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	if machineID != "" {
		query.Set("machine", machineID)
	}
	var out recording.List
	if err := c.do(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(teamID)+"/recordings?"+query.Encode(), nil, &out); err != nil {
		return recording.List{}, err
	}
	for _, r := range out.Recordings {
		if !recording.ValidRecordingID(r.ID) || r.MachineID == "" {
			return recording.List{}, errors.New("paperboat-server returned an invalid recording")
		}
	}
	return out, nil
}

// ReplayRecording asks for a replay ticket bound to this CLI session. The server writes the replay
// audit event before it answers.
func (c *Client) ReplayRecording(ctx context.Context, recordingID, cliClientSessionID string) (recording.Replay, error) {
	var out recording.Replay
	if err := c.do(ctx, http.MethodPost, "/v1/recordings/"+url.PathEscape(recordingID)+"/replay", recording.ReplayRequest{CLIClientSessionID: cliClientSessionID}, &out); err != nil {
		return recording.Replay{}, err
	}
	if out.Ticket == "" || out.MachineID == "" || out.TeamID == "" || out.KeyGeneration < 1 || out.ViewerKey.Generation < out.KeyGeneration || !recording.ValidSealed(out.ViewerKey.Sealed) {
		return recording.Replay{}, errInvalidReplay
	}
	return out, nil
}

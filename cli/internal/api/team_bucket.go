package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/pinksaucepasta/paperboat/shared/recording"
	"github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// TeamBucket is the team's own bucket (GET /v1/teams/{team}/bucket); the secret key is never returned.
func (c *Client) TeamBucket(ctx context.Context, teamID string) (teambucket.Config, error) {
	var out teambucket.Config
	err := c.do(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(teamID)+"/bucket", nil, &out)
	return out, err
}

// SetTeamBucket saves the bucket after the server checked it with a probe object.
func (c *Client) SetTeamBucket(ctx context.Context, teamID string, in teambucket.Input) (teambucket.Config, error) {
	var out teambucket.Config
	err := c.do(ctx, http.MethodPut, "/v1/teams/"+url.PathEscape(teamID)+"/bucket", in, &out)
	return out, err
}

// RemoveTeamBucket forgets the bucket; what it already holds stays there.
func (c *Client) RemoveTeamBucket(ctx context.Context, teamID string) error {
	var out struct{}
	return c.do(ctx, http.MethodDelete, "/v1/teams/"+url.PathEscape(teamID)+"/bucket", nil, &out)
}

// ReplayRecordingArchive asks for the presigned downloads of a recording's archived segments in the team bucket. The
// server writes the replay audit event before it answers; no machine is involved.
func (c *Client) ReplayRecordingArchive(ctx context.Context, recordingID string) (recording.Replay, error) {
	var out recording.Replay
	if err := c.do(ctx, http.MethodPost, "/v1/recordings/"+url.PathEscape(recordingID)+"/replay", recording.ReplayRequest{Source: recording.SourceBucket}, &out); err != nil {
		return recording.Replay{}, err
	}
	if out.Source != recording.SourceBucket || len(out.Segments) == 0 || out.TeamID == "" || out.KeyGeneration < 1 || out.ViewerKey.Generation < out.KeyGeneration || !recording.ValidSealed(out.ViewerKey.Sealed) {
		return recording.Replay{}, errInvalidReplay
	}
	return out, nil
}

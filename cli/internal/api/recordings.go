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

// recordingPageLimit is the most recordings one request may ask for; the server refuses more.
const recordingPageLimit = 200

// ErrRecordingListStalled reports that a later page held nothing new, so reading on would not reach the end.
var ErrRecordingListStalled = errors.New("paperboat-server repeated a page of recordings")

// ListRecordings reads at most maxItems of a team's recordings this account may see, in the server's order,
// one page at a time (GET /v1/teams/{team}/recordings?limit=&offset=). Any failed page fails the whole list.
func (c *Client) ListRecordings(ctx context.Context, teamID, machineID string, maxItems int) (recording.List, error) {
	out := recording.List{Recordings: []recording.Recording{}}
	// A recording created between two requests shifts the offsets, so a page may start with ones already read.
	seen := map[string]bool{}
	for offset := 0; len(out.Recordings) < maxItems; {
		// Near maxItems the page is not shrunk to what is missing: a small page could hold only repeats.
		page, err := c.recordingsPage(ctx, teamID, machineID, min(maxItems, recordingPageLimit), offset)
		if err != nil {
			return recording.List{}, err
		}
		out.Total = page.Total
		before := len(out.Recordings)
		for _, r := range page.Recordings {
			if !seen[r.ID] && len(out.Recordings) < maxItems {
				seen[r.ID] = true
				out.Recordings = append(out.Recordings, r)
			}
		}
		if len(page.Recordings) == 0 {
			break
		}
		if len(out.Recordings) == before {
			return recording.List{}, ErrRecordingListStalled
		}
		if offset += len(page.Recordings); int64(offset) >= page.Total {
			break
		}
	}
	return out, nil
}

func (c *Client) recordingsPage(ctx context.Context, teamID, machineID string, limit, offset int) (recording.List, error) {
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

// RecordingKeyState reads the team's recording key: its public chain head, who can replay and this account's own copy.
func (c *Client) RecordingKeyState(ctx context.Context, team string) (recording.KeyState, error) {
	var out recording.KeyState
	if err := c.do(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(team)+"/recording-key", nil, &out); err != nil {
		return recording.KeyState{}, err
	}
	return out, nil
}

// ReplaceRecordingKey posts the next generation of the team's recording key (recording.ReplaceKey). The server stores
// it only when its rotation signature verifies under the current generation's signing key.
func (c *Client) ReplaceRecordingKey(ctx context.Context, team string, request recording.KeyCreate) (recording.Key, error) {
	var out recording.Key
	if err := c.do(ctx, http.MethodPost, "/v1/teams/"+url.PathEscape(team)+"/recording-key", request, &out); err != nil {
		return recording.Key{}, err
	}
	return out, nil
}

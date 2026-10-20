package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const machineEventsResponseLimit = 4 << 10

// WaitMachineEvents holds one request open for up to the server's hold time on a machine of the
// signed-in account. known maps each topic to the generation last seen; the answer holds only the
// topics whose generation is higher, or nothing after the hold time. Generations carry no data.
func (c *Client) WaitMachineEvents(ctx context.Context, machineID string, known map[string]int64) (map[string]int64, error) {
	if machineID == "" {
		return nil, fmt.Errorf("machine events need a machine id")
	}
	if known == nil {
		known = map[string]int64{}
	}
	request, _, supportReference, path, err := c.newRequest(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/events/wait", struct {
		Topics map[string]int64 `json:"topics"`
	}{known}, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call POST %s: %w", path, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var answer struct {
			Topics map[string]int64 `json:"topics"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, machineEventsResponseLimit)).Decode(&answer); err != nil {
			return nil, &ResponseDecodeError{Err: err}
		}
		return answer.Topics, nil
	default:
		var envelope apiEnvelope
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, machineEventsResponseLimit)).Decode(&envelope)
		return nil, c.responseError(response, path, supportReference, envelope, decodeErr)
	}
}

package containers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// UpdateRequest is an update an operator asked this node for (Update in the console or admin).
type UpdateRequest struct {
	ID            string `json:"id"`
	TargetVersion string `json:"target_version"`
	State         string `json:"state"`
}

// ErrUpdateRequired means this pbh is below its update floor; the error text says why and how to update.
var ErrUpdateRequired = errors.New("this pbh release is below the minimum supported version")

// UpdateNode is how pbh talks to the server about its own update, with its node's runtime credential.
type UpdateNode struct{ node *nodeClient }

// OpenUpdateNode uses the installation's tunnel component, else its relay, to reach the server.
func OpenUpdateNode(dir string) (*UpdateNode, error) {
	var lastErr error
	for _, capability := range []string{"tunnel", "relay"} {
		if _, err := os.Stat(filepath.Join(dir, capability, "runtime.args")); err != nil {
			lastErr = err
			continue
		}
		node, err := newNodeClient(capability, filepath.Join(dir, capability))
		if err == nil {
			return &UpdateNode{node: node}, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("no claimed component to check updates with: %w", lastErr)
}

// ControlURL is the server this node is registered with; it also serves releases and their signed metadata.
func (u *UpdateNode) ControlURL() string { return strings.TrimRight(u.node.url, "/") }

// DownloadClient trusts the same server as the node, with a timeout for a whole bundle.
func (u *UpdateNode) DownloadClient() *http.Client {
	return &http.Client{Transport: u.node.http.Transport, Timeout: 10 * time.Minute, CheckRedirect: u.node.http.CheckRedirect}
}

// Check reports this pbh's version and returns the update to run, if any.
func (u *UpdateNode) Check(ctx context.Context, version string) (*UpdateRequest, error) {
	var out struct {
		Data struct {
			Update *UpdateRequest `json:"update"`
		} `json:"data"`
	}
	status, body, err := u.post(ctx, "/v1/selfhost/update/check", map[string]string{"version": version})
	if err != nil {
		return nil, err
	}
	switch {
	case status == http.StatusUpgradeRequired:
		var refusal struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		return nil, fmt.Errorf("%w: %s", ErrUpdateRequired, refusal.Error.Message)
	case status != http.StatusOK:
		return nil, ResponseError{Status: status}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errors.New("update check response is invalid")
	}
	return out.Data.Update, nil
}

// Report records one step of the running update, with the exact failure and its support reference.
func (u *UpdateNode) Report(ctx context.Context, id, state, detail, supportReference string) error {
	status, _, err := u.post(ctx, "/v1/selfhost/update/report", map[string]string{"id": id, "state": state, "detail": detail, "support_reference": supportReference})
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return ResponseError{Status: status}
	}
	return nil
}

func (u *UpdateNode) post(ctx context.Context, path string, in any) (int, []byte, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.ControlURL()+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.node.credential)
	req.Header.Set("Content-Type", "application/json")
	response, err := u.node.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	return response.StatusCode, body, err
}

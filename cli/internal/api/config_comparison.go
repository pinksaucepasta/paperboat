package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

type ConfigComparisonDescriptor struct {
	ExecDescriptor
	Binding configsync.ConflictComparisonRequest `json:"binding"`
}

func (c *Client) ConfigConflictConnection(ctx context.Context, environmentID, conflictRevision string, assignmentVersion int64, path, remote, source, cliSession string) (ConfigComparisonDescriptor, error) {
	var result ConfigComparisonDescriptor
	if environmentID == "" || conflictRevision == "" || assignmentVersion < 1 || path == "" || remote == "" || source == "" || cliSession == "" {
		return result, errors.New("exact conflict and client identity are required")
	}
	err := c.do(ctx, http.MethodPost, "/v1/config-sync/environments/"+url.PathEscape(environmentID)+"/conflicts/"+url.PathEscape(conflictRevision)+"/connection", map[string]any{"assignment_version": assignmentVersion, "path": path, "expected_remote_revision": remote, "source_machine_id": source, "cli_client_session_id": cliSession}, &result)
	if err != nil {
		return result, err
	}
	if len(result.OperationID) < 8 || len(result.OperationID) > 128 || !result.ExpiresAt.After(time.Now()) || result.ExpiresAt.Sub(time.Now()) > 5*time.Minute {
		return ConfigComparisonDescriptor{}, errors.New("invalid conflict comparison lifetime")
	}
	if result.Environment == nil {
		return ConfigComparisonDescriptor{}, errors.New("invalid conflict comparison environment")
	}
	if err := validateOperationDescriptor(result.ExecDescriptor, result.Environment.ResourceID, result.OperationID, "config:compare", "config comparison"); err != nil {
		return ConfigComparisonDescriptor{}, err
	}
	b := result.Binding
	if b.AssignmentID == "" || b.AssignmentVersion != assignmentVersion || b.Path != path || b.ConflictRevision != conflictRevision || b.ExpectedRemoteRevision != remote || result.Environment == nil || result.Environment.ID != environmentID || result.Environment.ResourceID == "" || result.Auth.AccessSessionID == "" || len(result.Auth.AccessSessionID) > 128 || result.Auth.Token == "" || result.Auth.Method != "bearer" || len(result.Auth.Scopes) != 1 || result.Auth.Scopes[0] != "config:compare" || result.ExpiresAt.IsZero() || !result.ExpiresAt.Equal(result.Auth.ExpiresAt) {
		return ConfigComparisonDescriptor{}, errors.New("invalid conflict comparison descriptor")
	}
	return result, nil
}

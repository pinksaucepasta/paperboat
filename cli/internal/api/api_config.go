package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

type AvailabilityPolicy struct {
	Schema             string     `json:"schema"`
	DesiredMode        string     `json:"desired_mode"`
	DesiredVersion     int64      `json:"desired_version"`
	ObservedMode       string     `json:"observed_mode,omitempty"`
	ObservedVersion    int64      `json:"observed_version"`
	ObservedAt         *time.Time `json:"observed_at,omitempty"`
	Status             string     `json:"status"`
	ErrorCode          string     `json:"error_code,omitempty"`
	HostServiceVersion string     `json:"host_service_version,omitempty"`
	HostServiceScope   string     `json:"host_service_scope,omitempty"`
	UpdateRollbacks    int64      `json:"update_rollbacks"`
	UpdateHealth       string     `json:"update_health"`
}

type ConfigRepository struct {
	State       string `json:"state"`
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	ExternalRef string `json:"external_ref"`
	DisplayName string `json:"display_name"`
}

type ConfigPathRule struct {
	Source         string   `json:"source"`
	ID             string   `json:"id"`
	RepositoryPath string   `json:"repository_path"`
	LocalPath      string   `json:"local_path"`
	Kind           string   `json:"kind"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
}

type ConfigRepositoryBinding struct {
	RepositoryID string `json:"repository_id"`
	URL          string `json:"url"`
}

type ConfigAssignment struct {
	ConfigurationRevision string                    `json:"configuration_revision"`
	PathRules             []ConfigPathRule          `json:"path_rules"`
	RepositoryBindings    []ConfigRepositoryBinding `json:"repository_bindings"`
	ID                    string                    `json:"id"`
	MachineID             string                    `json:"machine_id"`
	EnvironmentID         string                    `json:"environment_id"`
	RepositoryID          *string                   `json:"repository_id"`
	PullRepositoryID      *string                   `json:"pull_repository_id"`
	PushRepositoryID      *string                   `json:"push_repository_id"`
	AutomaticUpdates      bool                      `json:"automatic_updates"`
	ConsentState          string                    `json:"consent_state"`
	Mode                  string                    `json:"mode"`
	WarningRevision       *string                   `json:"warning_revision"`
	Version               int64                     `json:"version"`
}

type ConfigWarningFacts struct {
	Revision             string `json:"revision"`
	MachineName          string `json:"machine_name"`
	RepositoryName       string `json:"repository_name"`
	CanonicalScope       string `json:"canonical_scope"`
	Mode                 string `json:"mode"`
	ManifestScope        string `json:"manifest_scope"`
	RepositoryVisibility string `json:"repository_visibility"`
	HistoryRetention     string `json:"history_retention"`
	ConflictBehavior     string `json:"conflict_behavior"`
	ForceBehavior        string `json:"force_behavior"`
	DisableAction        string `json:"disable_action"`
	OfflineBehavior      string `json:"offline_behavior"`
	AccessConsequence    string `json:"access_consequence"`
}

type Favorite struct {
	Kind       string    `json:"kind"`
	ResourceID string    `json:"resource_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func (c *Client) ListFavorites(ctx context.Context) ([]Favorite, error) {
	var out []Favorite
	err := c.do(ctx, http.MethodGet, "/v1/favorites", nil, &out)
	return out, err
}

func (c *Client) SetFavorite(ctx context.Context, kind, resourceID string, favorite bool) ([]Favorite, error) {
	var out []Favorite
	err := c.do(ctx, http.MethodPut, "/v1/favorites", map[string]any{"kind": kind, "resource_id": resourceID, "favorite": favorite}, &out)
	return out, err
}

func (c *Client) ListConfigRepositories(ctx context.Context) ([]ConfigRepository, error) {
	return collectOffsetInventory[ConfigRepository](ctx, c, "/v1/config-repositories", "items", nil)
}

type ConfigRepositoryCandidate struct {
	Provider      string `json:"provider"`
	ExternalID    string `json:"external_id"`
	DisplayName   string `json:"display_name"`
	DefaultBranch string `json:"default_branch"`
}

func (c *Client) ConfigRepositoryCandidates(ctx context.Context) ([]ConfigRepositoryCandidate, error) {
	return collectCursorInventory[ConfigRepositoryCandidate](ctx, c, "/v1/config-repositories/candidates")
}

func (c *Client) ConnectConfigRepository(ctx context.Context, candidate ConfigRepositoryCandidate) (ConfigRepository, error) {
	var out ConfigRepository
	err := c.do(ctx, http.MethodPost, "/v1/config-repositories", map[string]string{"provider": candidate.Provider, "external_ref": candidate.ExternalID, "display_name": candidate.DisplayName}, &out)
	return out, err
}

func (c *Client) ConnectCustomConfigRepository(ctx context.Context, endpoint, displayName, branch string) (ConfigRepository, error) {
	var out ConfigRepository
	err := c.do(ctx, http.MethodPost, "/v1/config-repositories", map[string]string{"provider": "git", "external_ref": endpoint, "display_name": displayName, "branch": branch}, &out)
	return out, err
}

func (c *Client) ConfigRepositoryReadAccess(ctx context.Context, repositoryID, endpoint string) (configsync.RepositoryReadAccess, error) {
	var out configsync.RepositoryReadAccess
	err := c.do(ctx, http.MethodPost, "/v1/config-repositories/"+url.PathEscape(repositoryID)+"/read-access", map[string]string{"url": endpoint}, &out)
	return out, err
}

func (c *Client) ConfigAssignment(ctx context.Context, machineID string) (ConfigAssignment, error) {
	var out ConfigAssignment
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(machineID)+"/config-assignment", nil, &out)
	return out, err
}

func (c *Client) ConfigureConfigProjection(ctx context.Context, machineID, pullRepositoryID, pushRepositoryID, mode string, automaticUpdates bool, expectedVersion int64, rules []ConfigPathRule, bindings []ConfigRepositoryBinding, revision string) (ConfigAssignment, error) {
	if rules == nil {
		rules = []ConfigPathRule{}
	}
	if bindings == nil {
		bindings = []ConfigRepositoryBinding{}
	}
	var out ConfigAssignment
	err := c.do(ctx, http.MethodPut, "/v1/machines/"+url.PathEscape(machineID)+"/config-assignment", map[string]any{"pull_repository_id": pullRepositoryID, "push_repository_id": pushRepositoryID, "mode": mode, "automatic_updates": automaticUpdates, "warning_revision": "", "expected_version": expectedVersion, "path_rules": rules, "repository_bindings": bindings, "configuration_revision": revision}, &out)
	return out, err
}

func (c *Client) ApproveConfigPullRevision(ctx context.Context, machineID, revision string, expectedVersion int64) (ConfigAssignment, error) {
	var out ConfigAssignment
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/config-assignment/approve", map[string]any{"remote_revision": revision, "expected_version": expectedVersion}, &out)
	return out, err
}

type ConfigTeamDefault struct {
	TeamID               string `json:"team_id"`
	Provider             string `json:"provider"`
	ExternalRepositoryID string `json:"external_repository_id"`
	DisplayName          string `json:"display_name"`
	Branch               string `json:"branch"`
	Version              int64  `json:"version"`
	RepositoryID         string `json:"repository_id,omitempty"`
	AdoptedVersion       int64  `json:"adopted_version,omitempty"`
	UpdatePending        bool   `json:"update_pending,omitempty"`
}

func (c *Client) ConfigTeamDefault(ctx context.Context, teamID string) (ConfigTeamDefault, error) {
	var out ConfigTeamDefault
	err := c.do(ctx, http.MethodGet, "/v1/teams/"+url.PathEscape(teamID)+"/config-default", nil, &out)
	return out, err
}

func (c *Client) SetConfigTeamDefault(ctx context.Context, teamID, repositoryID string, expectedVersion int64) (ConfigTeamDefault, error) {
	var out ConfigTeamDefault
	err := c.do(ctx, http.MethodPut, "/v1/teams/"+url.PathEscape(teamID)+"/config-default", map[string]any{"repository_id": repositoryID, "expected_version": expectedVersion}, &out)
	return out, err
}

func (c *Client) AdoptConfigTeamDefault(ctx context.Context, teamID string, version int64) (ConfigTeamDefault, error) {
	var out ConfigTeamDefault
	err := c.do(ctx, http.MethodPut, "/v1/config-sync/team-default-adoption", map[string]any{"team_id": teamID, "default_version": version}, &out)
	return out, err
}

func (c *Client) UnadoptConfigTeamDefault(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/v1/config-sync/team-default-adoption", nil, nil)
}

func (c *Client) UnassignConfig(ctx context.Context, machineID string, expectedVersion int64) error {
	path := fmt.Sprintf("/v1/machines/%s/config-assignment?expected_version=%d", url.PathEscape(machineID), expectedVersion)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) ConfigWarning(ctx context.Context, machineID string) (ConfigWarningFacts, error) {
	var out ConfigWarningFacts
	err := c.do(ctx, http.MethodGet, "/v1/machines/"+url.PathEscape(machineID)+"/config-assignment/warning", nil, &out)
	return out, err
}

func (c *Client) AcceptConfigConsent(ctx context.Context, machineID, warningRevision string, expectedVersion int64) (ConfigAssignment, error) {
	var out ConfigAssignment
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/config-assignment/consent", map[string]any{"warning_revision": warningRevision, "expected_version": expectedVersion}, &out)
	return out, err
}

// ConfigSyncStatus is the account-wide status response. The CLI selects the
// entry matching the attached machine and intentionally ignores path/error
// details when rendering its local status line.
type ConfigSyncStatus struct {
	State        string                       `json:"state"`
	Environments []ConfigSyncEnvironmentState `json:"environments"`
}

type ConfigSyncEnvironmentState struct {
	MachineID             string                  `json:"machine_id"`
	EnvironmentID         string                  `json:"environment_id"`
	Alias                 string                  `json:"alias"`
	State                 string                  `json:"state"`
	Mode                  string                  `json:"mode"`
	AssignmentVersion     int64                   `json:"assignment_version"`
	RemoteRevision        string                  `json:"remote_revision"`
	ManifestHealth        string                  `json:"manifest_health"`
	ManifestRevision      string                  `json:"manifest_revision"`
	ManagedPathCount      int                     `json:"managed_path_count"`
	PendingCleanPathCount int                     `json:"pending_clean_path_count"`
	LastAppliedRevision   string                  `json:"last_applied_revision"`
	LastPublishedRevision string                  `json:"last_published_revision"`
	Conflicts             []ConfigSyncPathSummary `json:"conflicts"`
	Review                []ConfigSyncPathSummary `json:"review"`
	ErrorCode             string                  `json:"error_code,omitempty"`
	RecoveryActions       []string                `json:"recovery_actions,omitempty"`
}

type ConfigSyncPathSummary struct {
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes,omitempty"`
	Reason   string `json:"reason"`
	Revision string `json:"revision,omitempty"`
}

// ConfigSyncStatus gets the authenticated account's configuration sync state.
func (c *Client) ConfigSyncStatus(ctx context.Context) (ConfigSyncStatus, error) {
	var out ConfigSyncStatus
	err := c.do(ctx, http.MethodGet, "/v1/config-sync/status", nil, &out)
	return out, err
}

type ConfigConflictRequest struct {
	Path                      string `json:"path"`
	ConflictRevision          string `json:"conflict_revision"`
	ExpectedRemoteRevision    string `json:"expected_remote_revision"`
	ExpectedAssignmentVersion int64  `json:"expected_assignment_version"`
	Action                    string `json:"action"`
}

type ConfigOperation struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Action string `json:"action"`
}

func (c *Client) ResolveConfigConflict(ctx context.Context, machineID string, request ConfigConflictRequest) (ConfigOperation, error) {
	var out ConfigOperation
	err := c.do(ctx, http.MethodPost, "/v1/config-sync/environments/"+url.PathEscape(machineID)+"/conflict-resolutions", request, &out)
	return out, err
}

type ConfigForceRequest struct {
	Scope                     string `json:"scope"`
	Path                      string `json:"path,omitempty"`
	ConflictRevision          string `json:"conflict_revision,omitempty"`
	ExpectedRemoteRevision    string `json:"expected_remote_revision"`
	ExpectedAssignmentVersion int64  `json:"expected_assignment_version"`
	Action                    string `json:"action"`
	Confirmation              string `json:"confirmation"`
}

func (c *Client) ForceConfig(ctx context.Context, machineID string, request ConfigForceRequest) (ConfigOperation, error) {
	var out ConfigOperation
	err := c.do(ctx, http.MethodPost, "/v1/config-sync/environments/"+url.PathEscape(machineID)+"/force", request, &out)
	return out, err
}

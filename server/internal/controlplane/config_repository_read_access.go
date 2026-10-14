package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// ConfigRepositoryReadAccess authorizes reading repository configuration before
// mapped-root consent. It grants no machine filesystem or publication authority.
type ConfigRepositoryReadAccess struct {
	RepositoryID string    `json:"repository_id"`
	CloneURL     string    `json:"clone_url"`
	Branch       string    `json:"branch"`
	Transport    string    `json:"transport"`
	Username     string    `json:"username"`
	Password     string    `json:"password"`
	Capability   string    `json:"capability"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *ConfigRepositoryAccessService) IssueReadAccess(ctx context.Context, userID, repositoryID, endpoint string) (ConfigRepositoryReadAccess, error) {
	if s == nil || s.store == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(repositoryID) == "" {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	owned := dbsqlc.GetOwnedControlConfigRepositoryParams{ID: repositoryID, OwnerUserID: userID}
	repository, err := s.store.Queries().GetOwnedControlConfigRepository(ctx, owned)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	if err != nil {
		return ConfigRepositoryReadAccess{}, err
	}
	if repository.State != "active" || !repository.CloneUrl.Valid || !repository.DefaultBranch.Valid || !validConfigBranch(repository.DefaultBranch.String) {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	cloneURL := repository.CloneUrl.String
	custom := repository.Provider == "git" && repository.CredentialCapability.String == "machine_repository"
	if endpoint != "" {
		if !custom {
			return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
		}
		cloneURL = endpoint
	}
	transport, ok := ConfigRepositoryTransport(cloneURL)
	if !ok {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	now := s.clock().UTC()
	result := ConfigRepositoryReadAccess{RepositoryID: repository.ID, CloneURL: cloneURL, Branch: repository.DefaultBranch.String, Transport: transport, Capability: "repository_contents_read", ExpiresAt: now.Add(5 * time.Minute)}
	if !custom {
		if repository.Provider != "github" || !repository.ExternalRepositoryID.Valid || repository.ExternalRepositoryID.String == "" || !repository.AuthorizationRef.Valid || repository.AuthorizationRef.String == "" || repository.CredentialCapability.String != "provider_user_repository" || s.issuer == nil || !safeRepositoryCoordinate(cloneURL) || transport != "https" {
			return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
		}
		issued, issueErr := s.issuer.IssueRepositoryAccess(ctx, userID, repository.ExternalRepositoryID.String, "read")
		if issueErr != nil {
			return ConfigRepositoryReadAccess{}, errors.Join(ErrConfigRepositoryAccessUncertain, issueErr)
		}
		if issued.Token == "" || len(issued.Token) > 4096 || !issued.ExpiresAt.After(now) || issued.ExpiresAt.After(now.Add(time.Hour+time.Minute)) {
			return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessUncertain
		}
		result.Username, result.Password = "x-access-token", issued.Token
		if issued.ExpiresAt.Before(result.ExpiresAt) {
			result.ExpiresAt = issued.ExpiresAt.UTC()
		}
	}
	current, err := s.store.Queries().GetOwnedControlConfigRepository(ctx, owned)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	if err != nil {
		return ConfigRepositoryReadAccess{}, err
	}
	if current.State != "active" || current.Version != repository.Version {
		return ConfigRepositoryReadAccess{}, ErrConfigRepositoryAccessInvalid
	}
	return result, nil
}

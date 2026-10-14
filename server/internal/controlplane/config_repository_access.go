package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
)

var (
	ErrConfigRepositoryAccessInvalid         = errors.New("config repository access request is invalid")
	ErrConfigRepositoryAccessReplay          = errors.New("config repository access operation conflicts with an earlier request")
	ErrConfigRepositoryAccessUncertain       = errors.New("config repository access issuance outcome is uncertain")
	errInvalidScopedRepositoryCredential     = errors.New("provider returned an invalid scoped repository credential")
	errRepositoryAccessUncertainNotPersisted = errors.New("config repository access uncertainty was not persisted")
)

type ScopedRepositoryCredential struct {
	Token     string
	ExpiresAt time.Time
}

type ConfigRepositoryAccessIssuer interface {
	IssueRepositoryAccess(context.Context, string, string, string) (ScopedRepositoryCredential, error)
}

type ConfigRepositoryAccessIssuerFuncs struct {
	Issue func(context.Context, string, string, string) (ScopedRepositoryCredential, error)
}

func (f ConfigRepositoryAccessIssuerFuncs) IssueRepositoryAccess(ctx context.Context, userID, repositoryID, contentsPermission string) (ScopedRepositoryCredential, error) {
	return f.Issue(ctx, userID, repositoryID, contentsPermission)
}

type ConfigRepositoryAccess struct {
	RepositoryID  string    `json:"repository_id"`
	AssignmentID  string    `json:"assignment_id"`
	EnvironmentID string    `json:"environment_id"`
	MachineID     string    `json:"machine_id"`
	CloneURL      string    `json:"clone_url"`
	PublishURL    string    `json:"publish_url"`
	Branch        string    `json:"branch"`
	Transport     string    `json:"transport"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Capability    string    `json:"capability"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type ConfigRepositoryAccessService struct {
	store         *db.DB
	leases        *ConfigLeaseService
	issuer        ConfigRepositoryAccessIssuer
	encryptionKey string
	audit         *audit.Writer
	clock         func() time.Time
}

func NewConfigRepositoryAccessService(store *db.DB, leases *ConfigLeaseService, issuer ConfigRepositoryAccessIssuer, encryptionKey string, writer *audit.Writer) *ConfigRepositoryAccessService {
	return &ConfigRepositoryAccessService{
		store: store, leases: leases, issuer: issuer, encryptionKey: encryptionKey,
		audit: writer, clock: func() time.Time { return time.Now().UTC() },
	}
}

func (s *ConfigRepositoryAccessService) Issue(ctx context.Context, identityToken, configCredential string, proof, body []byte, method, path, operationID, direction string) (ConfigRepositoryAccess, error) {
	if s == nil || s.store == nil || s.leases == nil || s.encryptionKey == "" ||
		strings.TrimSpace(operationID) == "" || len(operationID) > 256 {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	holder, err := s.leases.Authenticate(ctx, identityToken, configCredential, proof, body, method, path)
	if err != nil {
		if errors.Is(err, ErrConfigLeaseInvalid) {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		return ConfigRepositoryAccess{}, err
	}
	direction = strings.TrimSpace(direction)
	if direction != "" && direction != "pull" && direction != "push" {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	contentsPermission := "write"
	capability := "repository_contents_write"
	assignment, err := s.store.Queries().GetControlConfigAssignment(ctx, holder.EnvironmentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		return ConfigRepositoryAccess{}, err
	}
	if assignment.ID != holder.AssignmentID {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	if direction == "" {
		if assignment.Mode == ConfigModePullOnly {
			direction = "pull"
		} else {
			direction = "push"
		}
	}
	if direction == "pull" {
		if assignment.Mode == ConfigModePushOnly || !assignment.RepositoryID.Valid {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		holder.RepositoryID = assignment.RepositoryID.String
		contentsPermission, capability = "read", "repository_contents_read"
	} else {
		if assignment.Mode == ConfigModePullOnly {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		if assignment.PushRepositoryID.Valid {
			holder.RepositoryID = assignment.PushRepositoryID.String
		}
	}
	if s.leases.mode == "read_only" {
		contentsPermission, capability = "read", "repository_contents_read"
	}
	requestHash := repositoryAccessRequestHash(operationID, holder, capability)
	now := s.clock().UTC()
	existing, err := s.store.Queries().GetControlConfigRepositoryAccessOperation(ctx, operationID)
	if err == nil {
		return s.replay(existing, requestHash, capability, now)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConfigRepositoryAccess{}, err
	}
	repository, err := s.store.Queries().GetActiveControlConfigRepository(ctx, holder.RepositoryID)
	environment, envErr := s.store.Queries().GetControlEnvironment(ctx, holder.EnvironmentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		return ConfigRepositoryAccess{}, err
	}
	if envErr != nil {
		if errors.Is(envErr, sql.ErrNoRows) {
			return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
		}
		return ConfigRepositoryAccess{}, envErr
	}
	if !environment.OwnerUserID.Valid || repository.OwnerUserID != environment.OwnerUserID.String || !repository.CloneUrl.Valid || !repository.PublishUrl.Valid || !repository.DefaultBranch.Valid {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	cloneURL, publishURL := repository.CloneUrl.String, repository.PublishUrl.String
	custom := repository.Provider == "git" && repository.CredentialCapability.String == "machine_repository"
	if custom {
		for _, binding := range ConfigAssignmentBindings(assignment.RepositoryBindings) {
			if binding.RepositoryID == repository.ID {
				cloneURL, publishURL = binding.URL, binding.URL
			}
		}
	} else if repository.Provider != "github" || !repository.AuthorizationRef.Valid || repository.AuthorizationRef.String == "" || !repository.ExternalRepositoryID.Valid || repository.ExternalRepositoryID.String == "" || repository.CredentialCapability.String != "provider_user_repository" || s.issuer == nil || !safeRepositoryCoordinate(cloneURL) || !safeRepositoryCoordinate(publishURL) {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	transport, ok := ConfigRepositoryTransport(cloneURL)
	publishTransport, publishOK := ConfigRepositoryTransport(publishURL)
	if !ok || !publishOK || transport != publishTransport {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		current, authErr := tx.Queries().GetEligibleControlConfigAssignment(ctx, dbsqlc.GetEligibleControlConfigAssignmentParams{EnvironmentID: holder.EnvironmentID, MachineID: holder.MachineID, InstallationGeneration: holder.InstallationGeneration})
		if authErr != nil {
			if errors.Is(authErr, sql.ErrNoRows) {
				return ErrConfigRepositoryAccessInvalid
			}
			return authErr
		}
		if current.ID != assignment.ID || current.Version != assignment.Version {
			return ErrConfigRepositoryAccessInvalid
		}
		_, reserveErr := tx.Queries().ReserveControlConfigRepositoryAccess(ctx, dbsqlc.ReserveControlConfigRepositoryAccessParams{
			OperationID: operationID, RequestHash: requestHash, RepositoryID: holder.RepositoryID,
			AssignmentID: holder.AssignmentID, EnvironmentID: holder.EnvironmentID, MachineID: holder.MachineID,
			InstallationGeneration: holder.InstallationGeneration, WarningRevision: current.WarningRevision.String,
		})
		return reserveErr
	})
	if errors.Is(err, sql.ErrNoRows) {
		existing, getErr := s.store.Queries().GetControlConfigRepositoryAccessOperation(ctx, operationID)
		if getErr != nil {
			return ConfigRepositoryAccess{}, getErr
		}
		return s.replay(existing, requestHash, capability, now)
	}
	if err != nil {
		return ConfigRepositoryAccess{}, err
	}

	// PB authenticates and fences this helper operation, while Git access
	// remains authorized by the user who connected the repository.
	issued := ScopedRepositoryCredential{ExpiresAt: now.Add(15 * time.Minute)}
	username := ""
	if !custom {
		var issueErr error
		issued, issueErr = s.issuer.IssueRepositoryAccess(ctx, repository.OwnerUserID, repository.ExternalRepositoryID.String, contentsPermission)
		if issueErr != nil {
			markErr := s.markProviderOutcomeUncertain(ctx, operationID, now)
			return ConfigRepositoryAccess{}, errors.Join(ErrConfigRepositoryAccessUncertain, issueErr, markErr)
		}
		if issued.Token == "" || len(issued.Token) > 4096 || !issued.ExpiresAt.After(now.Add(time.Minute)) || issued.ExpiresAt.After(now.Add(time.Hour+time.Minute)) {
			markErr := s.markProviderOutcomeUncertain(ctx, operationID, now)
			return ConfigRepositoryAccess{}, errors.Join(ErrConfigRepositoryAccessUncertain, errInvalidScopedRepositoryCredential, markErr)
		}
		username = "x-access-token"
	}
	result := ConfigRepositoryAccess{RepositoryID: holder.RepositoryID, AssignmentID: holder.AssignmentID, EnvironmentID: holder.EnvironmentID, MachineID: holder.MachineID, CloneURL: cloneURL, PublishURL: publishURL, Branch: repository.DefaultBranch.String, Transport: transport, Username: username, Password: issued.Token, Capability: capability, ExpiresAt: issued.ExpiresAt.UTC()}
	encoded, _ := json.Marshal(result)
	ciphertext, err := secrets.Encrypt(s.encryptionKey, string(encoded))
	if err != nil {
		return ConfigRepositoryAccess{}, err
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		current, authErr := tx.Queries().GetEligibleControlConfigAssignment(ctx, dbsqlc.GetEligibleControlConfigAssignmentParams{EnvironmentID: holder.EnvironmentID, MachineID: holder.MachineID, InstallationGeneration: holder.InstallationGeneration})
		currentRepository, repoErr := tx.Queries().GetActiveControlConfigRepository(ctx, holder.RepositoryID)
		if authErr != nil {
			if errors.Is(authErr, sql.ErrNoRows) {
				return ErrConfigRepositoryAccessInvalid
			}
			return authErr
		}
		if repoErr != nil {
			if errors.Is(repoErr, sql.ErrNoRows) {
				return ErrConfigRepositoryAccessInvalid
			}
			return repoErr
		}
		if current.ID != assignment.ID || current.Version != assignment.Version || currentRepository.Version != repository.Version {
			return ErrConfigRepositoryAccessInvalid
		}
		if _, err := tx.Queries().CompleteControlConfigRepositoryAccess(ctx, dbsqlc.CompleteControlConfigRepositoryAccessParams{
			AccessCiphertext: ciphertext, ExpiresAt: sql.NullTime{Time: issued.ExpiresAt.UTC(), Valid: true}, Now: now, OperationID: operationID,
		}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "config.repository_access_issued",
			ResourceType: "config_repository", ResourceID: holder.RepositoryID,
			IdempotencyKey: "config.repository_access:" + operationID,
			Metadata:       map[string]any{"assignment_id": holder.AssignmentID, "environment_id": holder.EnvironmentID, "machine_id": holder.MachineID, "expires_at": issued.ExpiresAt.UTC()}})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	return result, err
}

func (s *ConfigRepositoryAccessService) markProviderOutcomeUncertain(ctx context.Context, operationID string, now time.Time) error {
	rows, err := s.store.Queries().MarkControlConfigRepositoryAccessUncertain(ctx, dbsqlc.MarkControlConfigRepositoryAccessUncertainParams{
		LastErrorCode: sql.NullString{String: "provider_outcome_uncertain", Valid: true},
		Now:           now,
		OperationID:   operationID,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return errRepositoryAccessUncertainNotPersisted
	}
	return nil
}

func (s *ConfigRepositoryAccessService) replay(row dbsqlc.ControlConfigRepositoryAccessOperation, requestHash []byte, capability string, now time.Time) (ConfigRepositoryAccess, error) {
	if !bytes.Equal(row.RequestHash, requestHash) {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessReplay
	}
	if row.State == "pending" || row.State == "uncertain" {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessUncertain
	}
	if row.State != "issued" || row.RevokedAt.Valid || !row.ExpiresAt.Valid || !row.ExpiresAt.Time.After(now) {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	plaintext, err := secrets.Decrypt(s.encryptionKey, row.AccessCiphertext)
	if err != nil {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	var result ConfigRepositoryAccess
	decoder := json.NewDecoder(strings.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !validRepositoryAccessResult(result) || result.Capability != capability || !result.ExpiresAt.After(now) {
		return ConfigRepositoryAccess{}, ErrConfigRepositoryAccessInvalid
	}
	return result, nil
}

func repositoryAccessRequestHash(operationID string, holder ConfigLeaseHolder, capability string) []byte {
	encoded, _ := json.Marshal(struct {
		OperationID string
		Holder      ConfigLeaseHolder
		Capability  string
	}{operationID, holder, capability})
	sum := sha256.Sum256(encoded)
	return sum[:]
}

func safeRepositoryCoordinate(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.User == nil && parsed.Hostname() != "" &&
		parsed.RawQuery == "" && parsed.Fragment == "" && strings.HasSuffix(parsed.Path, ".git")
}

func currentWarningRevision(ctx context.Context, store *db.DB, environmentID string) string {
	assignment, err := store.Queries().GetControlConfigAssignment(ctx, environmentID)
	if err != nil || !assignment.WarningRevision.Valid {
		return ""
	}
	return assignment.WarningRevision.String
}

func (s *ConfigRepositoryAccessService) RevocationWorker(interval time.Duration, batchSize int32) func(context.Context) error {
	if interval <= 0 {
		interval = time.Minute
	}
	if batchSize < 1 || batchSize > 100 {
		batchSize = 25
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finishProgress := workers.BeginIteration(ctx, interval)
			cycleErr := func() error {
				if err := s.revokeProviderAccess(ctx, batchSize); err != nil {
					return err
				}

				return nil
			}()
			finishProgress(cycleErr)
			if cycleErr != nil {
				return cycleErr
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

func (s *ConfigRepositoryAccessService) revokeProviderAccess(ctx context.Context, batchSize int32) error {
	now := s.clock().UTC()
	rows, err := s.store.Queries().ListControlConfigRepositoryAccessPendingProviderRevoke(ctx, dbsqlc.ListControlConfigRepositoryAccessPendingProviderRevokeParams{Now: sql.NullTime{Time: now, Valid: true}, RowLimit: batchSize})
	if err != nil {
		return err
	}
	for _, row := range rows {
		plaintext, decryptErr := secrets.Decrypt(s.encryptionKey, row.AccessCiphertext)
		var access ConfigRepositoryAccess
		if decryptErr == nil {
			decryptErr = json.Unmarshal([]byte(plaintext), &access)
		}
		if decryptErr != nil || !validRepositoryAccessResult(access) {
			_, _ = s.store.Queries().RecordControlConfigRepositoryAccessRevokeFailure(ctx, dbsqlc.RecordControlConfigRepositoryAccessRevokeFailureParams{
				LastErrorCode: sql.NullString{String: "access_decrypt_failed", Valid: true}, Now: now, OperationID: row.OperationID,
			})
			continue
		}
		// Never revoke the underlying user credential: it can authorize unrelated
		// provider workflows. Revocation here only retires PB's encrypted helper
		// copy; the provider owns external access revocation.
		if _, err := s.store.Queries().MarkControlConfigRepositoryAccessProviderRevoked(ctx, dbsqlc.MarkControlConfigRepositoryAccessProviderRevokedParams{Now: sql.NullTime{Time: now, Valid: true}, OperationID: row.OperationID}); err != nil {
			return err
		}
	}
	return nil
}

func validRepositoryAccessResult(a ConfigRepositoryAccess) bool {
	transport, ok := ConfigRepositoryTransport(a.CloneURL)
	publishTransport, publishOK := ConfigRepositoryTransport(a.PublishURL)
	return ok && publishOK && transport == publishTransport && transport == a.Transport && a.RepositoryID != "" && a.Branch != "" && ((a.Username == "" && a.Password == "") || (a.Transport == "https" && a.Username == "x-access-token" && a.Password != ""))
}

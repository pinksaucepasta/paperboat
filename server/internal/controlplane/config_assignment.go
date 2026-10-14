package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/workers"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

var (
	ErrAssignmentInvalid     = errors.New("config assignment is invalid")
	ErrAssignmentConflict    = errors.New("config assignment version conflict")
	ErrAssignmentForbidden   = errors.New("config assignment is unavailable")
	ErrAssignmentUnavailable = errors.New("config assignment service is unavailable")
)

const (
	ConfigModePullOnly      = "pull_only"
	ConfigModePushOnly      = "push_only"
	ConfigModeBidirectional = "bidirectional"
)

func validConfigMode(mode string) bool {
	return mode == ConfigModePullOnly || mode == ConfigModePushOnly || mode == ConfigModeBidirectional
}

type ConfigAssignmentService struct {
	store              *db.DB
	audit              *audit.Writer
	clock              func() time.Time
	warningRevision    string
	repositoryResolver ConfigRepositoryResolver
	repositoryCatalog  ConfigRepositoryCatalog
}

type ConfigRepositoryCandidate struct {
	Provider      string `json:"provider"`
	ExternalID    string `json:"external_id"`
	DisplayName   string `json:"display_name"`
	DefaultBranch string `json:"default_branch"`
}

type ConfigRepositoryCatalog interface {
	ListConfigRepositoryCandidates(context.Context, string) ([]ConfigRepositoryCandidate, error)
}

type ConfigRepositoryCatalogFunc func(context.Context, string) ([]ConfigRepositoryCandidate, error)

func (f ConfigRepositoryCatalogFunc) ListConfigRepositoryCandidates(ctx context.Context, userID string) ([]ConfigRepositoryCandidate, error) {
	return f(ctx, userID)
}

func (s *ConfigAssignmentService) SetRepositoryCatalog(catalog ConfigRepositoryCatalog) {
	s.repositoryCatalog = catalog
}

type ConfigRepositoryCatalogPager struct {
	List func(context.Context, string) ([]ConfigRepositoryCandidate, error)
	Page func(context.Context, string, string, int) ([]ConfigRepositoryCandidate, string, error)
}

func (c ConfigRepositoryCatalogPager) ListConfigRepositoryCandidates(ctx context.Context, account string) ([]ConfigRepositoryCandidate, error) {
	return c.List(ctx, account)
}
func (c ConfigRepositoryCatalogPager) ListConfigRepositoryCandidatePage(ctx context.Context, account, cursor string, limit int) ([]ConfigRepositoryCandidate, string, error) {
	return c.Page(ctx, account, cursor, limit)
}

type ConfigRepositoryPageCatalog interface {
	ListConfigRepositoryCandidatePage(context.Context, string, string, int) ([]ConfigRepositoryCandidate, string, error)
}

func (s *ConfigAssignmentService) RepositoryCandidateInventory(ctx context.Context, account, cursor string, limit int) ([]ConfigRepositoryCandidate, string, error) {
	if account == "" || limit < 1 || limit > 100 {
		return nil, "", ErrAssignmentForbidden
	}
	catalog, ok := s.repositoryCatalog.(ConfigRepositoryPageCatalog)
	if !ok {
		return nil, "", ErrAssignmentUnavailable
	}
	return catalog.ListConfigRepositoryCandidatePage(ctx, account, cursor, limit)
}
func (s *ConfigAssignmentService) RepositoryCandidates(ctx context.Context, userID string) ([]ConfigRepositoryCandidate, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrAssignmentForbidden
	}
	if s == nil || s.repositoryCatalog == nil {
		return nil, ErrAssignmentUnavailable
	}
	items, err := s.repositoryCatalog.ListConfigRepositoryCandidates(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(items) > 1000 {
		return nil, ErrAssignmentUnavailable
	}
	return items, nil
}

func NewConfigAssignmentService(store *db.DB, writer *audit.Writer, warningRevision string) *ConfigAssignmentService {
	return &ConfigAssignmentService{store: store, audit: writer, clock: func() time.Time { return time.Now().UTC() }, warningRevision: strings.TrimSpace(warningRevision)}
}

type ConfigRepositoryConnection struct {
	ProviderAccountID    string
	ExternalRepositoryID string
	DisplayName          string
	CloneURL             string
	PublishURL           string
	DefaultBranch        string
	AuthorizationRef     string
	CredentialCapability string
	ObservedRevision     string
}

type ConfigRepositoryResolver interface {
	ResolveConfigRepository(context.Context, string, string, string) (ConfigRepositoryConnection, error)
}

type ConfigRepositoryResolverFunc func(context.Context, string, string, string) (ConfigRepositoryConnection, error)

func (f ConfigRepositoryResolverFunc) ResolveConfigRepository(ctx context.Context, userID, provider, externalID string) (ConfigRepositoryConnection, error) {
	return f(ctx, userID, provider, externalID)
}

func (s *ConfigAssignmentService) SetRepositoryResolver(resolver ConfigRepositoryResolver) {
	s.repositoryResolver = resolver
}

func (s *ConfigAssignmentService) ConnectRepository(ctx context.Context, userID, provider, externalRef, displayName string, branch ...string) (dbsqlc.ControlConfigRepository, error) {
	provider, externalRef = strings.ToLower(strings.TrimSpace(provider)), strings.TrimSpace(externalRef)
	if userID == "" || provider == "" || externalRef == "" {
		return dbsqlc.ControlConfigRepository{}, ErrAssignmentInvalid
	}
	var connection ConfigRepositoryConnection
	var err error
	if provider == "git" {
		if _, ok := ConfigRepositoryTransport(externalRef); !ok || !safeCoordinateText(displayName) || len(displayName) > 128 {
			return dbsqlc.ControlConfigRepository{}, ErrAssignmentInvalid
		}
		defaultBranch := "main"
		if len(branch) > 0 && branch[0] != "" {
			defaultBranch = branch[0]
		}
		if !validConfigBranch(defaultBranch) {
			return dbsqlc.ControlConfigRepository{}, ErrAssignmentInvalid
		}
		connection = ConfigRepositoryConnection{DisplayName: displayName, CloneURL: externalRef, PublishURL: externalRef, DefaultBranch: defaultBranch, CredentialCapability: "machine_repository"}
	} else {
		if len(externalRef) > 128 {
			return dbsqlc.ControlConfigRepository{}, ErrAssignmentInvalid
		}
		if s.repositoryResolver == nil {
			return dbsqlc.ControlConfigRepository{}, ErrAssignmentUnavailable
		}
		connection, err = s.repositoryResolver.ResolveConfigRepository(ctx, userID, provider, externalRef)
		if err != nil {
			return dbsqlc.ControlConfigRepository{}, err
		}
		if strings.TrimSpace(connection.ExternalRepositoryID) != externalRef || strings.TrimSpace(connection.ProviderAccountID) == "" || strings.TrimSpace(connection.DisplayName) == "" || len(connection.DisplayName) > 128 || strings.TrimSpace(connection.CloneURL) == "" || strings.TrimSpace(connection.PublishURL) == "" || strings.TrimSpace(connection.DefaultBranch) == "" || strings.TrimSpace(connection.AuthorizationRef) == "" || strings.TrimSpace(connection.CredentialCapability) == "" {
			return dbsqlc.ControlConfigRepository{}, ErrAssignmentForbidden
		}
	}
	id, err := randomHex("repository")
	if err != nil {
		return dbsqlc.ControlConfigRepository{}, err
	}
	item, err := s.store.Queries().CreateControlConfigRepository(ctx, dbsqlc.CreateControlConfigRepositoryParams{
		ID: id, OwnerUserID: userID, Provider: provider, ExternalRef: externalRef, DisplayName: strings.TrimSpace(connection.DisplayName),
		ProviderAccountID: nullableString(connection.ProviderAccountID), ExternalRepositoryID: nullableString(connection.ExternalRepositoryID),
		CloneUrl: nullableString(connection.CloneURL), PublishUrl: nullableString(connection.PublishURL),
		DefaultBranch: nullableString(connection.DefaultBranch), AuthorizationRef: nullableString(connection.AuthorizationRef),
		CredentialCapability: nullableString(connection.CredentialCapability), ObservedRevision: nullableString(connection.ObservedRevision),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigRepository{}, ErrAssignmentConflict
	}
	if err != nil {
		return dbsqlc.ControlConfigRepository{}, err
	}
	if err := s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.repository_connected",
		ResourceType: "config_repository", ResourceID: item.ID, IdempotencyKey: "config.repository.connect:" + item.ID,
		Metadata: map[string]any{"provider": provider, "external_repository_id": item.ExternalRepositoryID.String}}); err != nil {
		return dbsqlc.ControlConfigRepository{}, err
	}
	return item, nil
}

func (s *ConfigAssignmentService) ListRepositories(ctx context.Context, userID string, limit, offset int32) ([]dbsqlc.ControlConfigRepository, error) {
	if userID == "" || limit < 1 || limit > 100 || offset < 0 {
		return nil, ErrAssignmentInvalid
	}
	return s.store.Queries().ListControlConfigRepositories(ctx, dbsqlc.ListControlConfigRepositoriesParams{OwnerUserID: userID, RowLimit: limit, RowOffset: offset})
}

func (s *ConfigAssignmentService) DisconnectRepository(ctx context.Context, userID, repositoryID string) error {
	if userID == "" || repositoryID == "" {
		return ErrAssignmentInvalid
	}
	now := s.clock().UTC()
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		item, err := tx.Queries().DisconnectOwnedControlConfigRepository(ctx, dbsqlc.DisconnectOwnedControlConfigRepositoryParams{
			Now: sql.NullTime{Time: now, Valid: true}, ID: repositoryID, OwnerUserID: userID,
		})
		if err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigCredentialsForRepository(ctx, dbsqlc.RevokeControlConfigCredentialsForRepositoryParams{
			Now: sql.NullTime{Time: now, Valid: true}, RepositoryID: sql.NullString{String: repositoryID, Valid: true},
		}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryAccessForRepository(ctx, dbsqlc.RevokeControlConfigRepositoryAccessForRepositoryParams{
			Now: now, RepositoryID: repositoryID,
		}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryLease(ctx, dbsqlc.RevokeControlConfigRepositoryLeaseParams{
			Now: sql.NullTime{Time: now, Valid: true}, RepositoryID: repositoryID,
		}); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.Queries().DisableControlConfigAssignmentsForRepository(ctx, dbsqlc.DisableControlConfigAssignmentsForRepositoryParams{
			Now: sql.NullTime{Time: now, Valid: true}, RepositoryID: sql.NullString{String: repositoryID, Valid: true},
		}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.repository_disconnected",
			ResourceType: "config_repository", ResourceID: repositoryID, IdempotencyKey: "config.repository.disconnect:" + repositoryID + ":" + strconv.FormatInt(item.Version, 10),
			Metadata: map[string]any{"provider": item.Provider}})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAssignmentForbidden
	}
	return err
}

func (s *ConfigAssignmentService) Assignment(ctx context.Context, userID, machineID string) (dbsqlc.ControlConfigAssignment, error) {
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	assignment, err := s.store.Queries().GetControlConfigAssignment(ctx, environmentID)
	if errors.Is(err, sql.ErrNoRows) {
		// Ownership is already checked. An unassigned machine is a valid
		// disabled configuration, not a missing or forbidden machine.
		return dbsqlc.ControlConfigAssignment{MachineID: machineID, EnvironmentID: environmentID}, nil
	}
	return assignment, err
}

func (s *ConfigAssignmentService) Assign(ctx context.Context, userID, machineID, repositoryID, mode, warningRevision string, expectedVersion int64) (dbsqlc.ControlConfigAssignment, error) {
	return s.AssignTargets(ctx, userID, machineID, repositoryID, repositoryID, mode, false, warningRevision, expectedVersion)
}

func (s *ConfigAssignmentService) AssignTargets(ctx context.Context, userID, machineID, pullRepositoryID, pushRepositoryID, mode string, automaticUpdates bool, warningRevision string, expectedVersion int64) (dbsqlc.ControlConfigAssignment, error) {
	return s.assignProjection(ctx, userID, machineID, pullRepositoryID, pushRepositoryID, mode, automaticUpdates, warningRevision, expectedVersion, nil, nil, "")
}

// ConfigureProjection approves the effective file configuration supplied by the owner.
func (s *ConfigAssignmentService) ConfigureProjection(ctx context.Context, userID, machineID, pullRepositoryID, pushRepositoryID, mode string, automaticUpdates bool, warningRevision string, expectedVersion int64, rules []ConfigPathRule, bindings []ConfigRepositoryBinding, configurationRevision string) (dbsqlc.ControlConfigAssignment, error) {
	if !validConfigurationRevision(configurationRevision) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentInvalid
	}
	return s.assignProjection(ctx, userID, machineID, pullRepositoryID, pushRepositoryID, mode, automaticUpdates, warningRevision, expectedVersion, rules, bindings, configurationRevision)
}

func (s *ConfigAssignmentService) assignProjection(ctx context.Context, userID, machineID, pullRepositoryID, pushRepositoryID, mode string, automaticUpdates bool, warningRevision string, expectedVersion int64, rules []ConfigPathRule, bindings []ConfigRepositoryBinding, configurationRevision string) (dbsqlc.ControlConfigAssignment, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		mode = ConfigModePullOnly
	}
	pullRepositoryID, pushRepositoryID = strings.TrimSpace(pullRepositoryID), strings.TrimSpace(pushRepositoryID)
	if mode == ConfigModePullOnly {
		pushRepositoryID = ""
	}
	if mode == ConfigModePushOnly {
		pullRepositoryID = ""
	}
	if userID == "" || machineID == "" || !validConfigMode(mode) || expectedVersion < 0 ||
		(mode != ConfigModePushOnly && pullRepositoryID == "") || (mode != ConfigModePullOnly && pushRepositoryID == "") {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentInvalid
	}
	if err := ValidateConfigMachineRules(rules, bindings, pullRepositoryID, pushRepositoryID); err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	if rules == nil {
		rules = []ConfigPathRule{}
	}
	if bindings == nil {
		bindings = []ConfigRepositoryBinding{}
	}
	for i := range rules {
		if rules[i].Include == nil {
			rules[i].Include = []string{}
		}
		if rules[i].Exclude == nil {
			rules[i].Exclude = []string{}
		}
	}
	rulesJSON, _ := json.Marshal(rules)
	bindingsJSON, _ := json.Marshal(bindings)
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	environment, err := s.store.Queries().GetControlEnvironment(ctx, environmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentForbidden
	}
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	if !environment.OwnerUserID.Valid || environment.OwnerUserID.String != userID || environment.DesiredState != "active" {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentForbidden
	}
	for _, repositoryID := range []string{pullRepositoryID, pushRepositoryID} {
		if repositoryID == "" {
			continue
		}
		repository, err := s.store.Queries().GetOwnedControlConfigRepository(ctx, dbsqlc.GetOwnedControlConfigRepositoryParams{ID: repositoryID, OwnerUserID: userID})
		if errors.Is(err, sql.ErrNoRows) {
			return dbsqlc.ControlConfigAssignment{}, ErrAssignmentForbidden
		}
		if err != nil {
			return dbsqlc.ControlConfigAssignment{}, err
		}
		for _, binding := range bindings {
			if binding.RepositoryID == repositoryID && (repository.Provider != "git" || repository.CredentialCapability.String != "machine_repository") {
				return dbsqlc.ControlConfigAssignment{}, ErrAssignmentInvalid
			}
		}
	}
	consent := "pending"
	assignmentID, err := randomHex("assignment")
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	warningRevision = strings.TrimSpace(warningRevision)
	if s.warningRevision == "" || (warningRevision != "" && warningRevision != s.warningRevision) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	warningRevision = s.warningRevision
	var assignment dbsqlc.ControlConfigAssignment
	now := s.clock().UTC()
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		primaryRepositoryID := pullRepositoryID
		if primaryRepositoryID == "" {
			primaryRepositoryID = pushRepositoryID
		}
		assignment, err = tx.Queries().SetControlConfigAssignment(ctx, dbsqlc.SetControlConfigAssignmentParams{
			AssignmentID: assignmentID, EnvironmentID: environmentID,
			RepositoryID:     sql.NullString{String: primaryRepositoryID, Valid: true},
			PushRepositoryID: sql.NullString{String: pushRepositoryID, Valid: pushRepositoryID != ""},
			Mode:             mode, ConsentState: consent, WarningRevision: sql.NullString{String: warningRevision, Valid: true},
			AutomaticUpdates: automaticUpdates, ExpectedVersion: expectedVersion, Now: now, PathRules: rulesJSON, RepositoryBindings: bindingsJSON, ConfigurationRevision: configurationRevision,
		})
		if err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigCredentialsForEnvironment(ctx, dbsqlc.RevokeControlConfigCredentialsForEnvironmentParams{EnvironmentID: environmentID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryAccessForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryAccessForEnvironmentParams{EnvironmentID: environmentID, Now: now}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryLeasesForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryLeasesForEnvironmentParams{EnvironmentID: sql.NullString{String: environmentID, Valid: true}, Now: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.assignment_set", ResourceType: "environment", ResourceID: environmentID, IdempotencyKey: "config.assignment:" + environmentID + ":" + strconv.FormatInt(assignment.Version, 10), Metadata: map[string]any{"pull_repository_id": pullRepositoryID, "push_repository_id": pushRepositoryID, "mode": mode, "automatic_updates": automaticUpdates, "consent_state": consent}})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	return assignment, nil
}

func (s *ConfigAssignmentService) AcceptConsent(ctx context.Context, userID, machineID, warningRevision string, expectedVersion int64) (dbsqlc.ControlConfigAssignment, error) {
	if userID == "" || machineID == "" || warningRevision == "" || expectedVersion < 1 {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentInvalid
	}
	if warningRevision != s.warningRevision {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	assignment, err := s.store.Queries().AcceptControlConfigConsent(ctx, dbsqlc.AcceptControlConfigConsentParams{EnvironmentID: environmentID, WarningRevision: sql.NullString{String: warningRevision, Valid: true}, ExpectedVersion: expectedVersion, Now: sql.NullTime{Time: s.clock().UTC(), Valid: true}})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	return assignment, s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.consent_accepted", ResourceType: "environment", ResourceID: environmentID, IdempotencyKey: "config.consent:" + environmentID + ":" + strconv.FormatInt(assignment.Version, 10), Metadata: map[string]any{"warning_revision": warningRevision}})
}

func (s *ConfigAssignmentService) ApprovePullRevision(ctx context.Context, userID, machineID, revision string, expectedVersion int64) (dbsqlc.ControlConfigAssignment, error) {
	revision = strings.TrimSpace(revision)
	if len(revision) != 40 || expectedVersion < 1 {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentInvalid
	}
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	item, err := s.store.Queries().ApproveControlConfigPullRevision(ctx, dbsqlc.ApproveControlConfigPullRevisionParams{
		RemoteRevision: sql.NullString{String: revision, Valid: true}, Now: sql.NullTime{Time: s.clock().UTC(), Valid: true}, EnvironmentID: environmentID, ExpectedVersion: expectedVersion,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	return item, s.audit.Write(ctx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser,
		EventType: "config.pull_revision_approved", ResourceType: "environment", ResourceID: environmentID,
		IdempotencyKey: "config.pull_revision_approved:" + item.ID + ":" + strconv.FormatInt(item.Version, 10),
		Metadata:       map[string]any{"remote_revision": revision}})
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

func (s *ConfigAssignmentService) Warning(ctx context.Context, userID, machineID string) (ConfigWarningFacts, error) {
	if userID == "" || machineID == "" || s.warningRevision == "" {
		return ConfigWarningFacts{}, ErrAssignmentInvalid
	}
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return ConfigWarningFacts{}, err
	}
	item, err := s.store.Queries().GetControlConfigWarningContext(ctx, dbsqlc.GetControlConfigWarningContextParams{
		EnvironmentID: environmentID, OwnerUserID: sql.NullString{String: userID, Valid: true},
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ConfigWarningFacts{}, ErrAssignmentForbidden
	}
	if err != nil {
		return ConfigWarningFacts{}, err
	}
	return ConfigWarningFacts{
		Revision: s.warningRevision, MachineName: item.MachineName, RepositoryName: item.RepositoryName,
		CanonicalScope: item.CanonicalScope, Mode: item.Mode,
		ManifestScope:        "Only files selected by the effective .paperboat/config-sync.toml TOML shared defaults, matching OS section, and this machine's TOML config file are managed. Machine fields override matching OS fields, which override shared defaults. Destinations must be explicit; they are never inferred. Exclusions narrow the selected scope.",
		RepositoryVisibility: "Selected configuration content and history are ordinary plaintext in the connected Git repository. Repository visibility and storage access are controlled by its owner; custom repositories are not guaranteed private.",
		HistoryRetention:     "Git history can retain earlier and removed versions after files are changed, un-managed, or deleted.",
		ConflictBehavior:     "Only conflicting paths pause; choose this machine's version or the repository version while unrelated paths continue.",
		ForceBehavior:        "Confirmed force pull or force push can replace selected managed paths while preserving displaced versions in normal history.",
		DisableAction:        "Remove consent or unassign the repository to stop synchronization immediately.",
		OfflineBehavior:      "Offline changes remain local; synchronization requires fresh server authorization after reconnecting.",
		AccessConsequence:    "Anyone who gains repository, provider-account, Git server, or underlying filesystem/mount access may read selected configuration content and retained history.",
	}, nil
}

func (s *ConfigAssignmentService) RemoveConsent(ctx context.Context, userID, machineID string, expectedVersion int64) (dbsqlc.ControlConfigAssignment, error) {
	if userID == "" || machineID == "" || expectedVersion < 1 || s.warningRevision == "" {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentForbidden
	}
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return dbsqlc.ControlConfigAssignment{}, err
	}
	now := s.clock().UTC()
	var assignment dbsqlc.ControlConfigAssignment
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		assignment, err = tx.Queries().RemoveControlConfigConsent(ctx, dbsqlc.RemoveControlConfigConsentParams{
			WarningRevision: sql.NullString{String: s.warningRevision, Valid: true}, Now: now,
			EnvironmentID: environmentID, ExpectedVersion: expectedVersion,
		})
		if err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigCredentialsForEnvironment(ctx, dbsqlc.RevokeControlConfigCredentialsForEnvironmentParams{EnvironmentID: environmentID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryAccessForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryAccessForEnvironmentParams{EnvironmentID: environmentID, Now: now}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryLeasesForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryLeasesForEnvironmentParams{EnvironmentID: sql.NullString{String: environmentID, Valid: true}, Now: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.consent_removed",
			ResourceType: "environment", ResourceID: environmentID, IdempotencyKey: "config.consent.remove:" + environmentID + ":" + strconv.FormatInt(assignment.Version, 10),
			Metadata: map[string]any{"warning_revision": s.warningRevision}})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsqlc.ControlConfigAssignment{}, ErrAssignmentConflict
	}
	return assignment, err
}

func (s *ConfigAssignmentService) ReconcileWarning(ctx context.Context) ([]string, error) {
	if s == nil || s.store == nil || s.warningRevision == "" {
		return nil, ErrAssignmentInvalid
	}
	now := s.clock().UTC()
	var environments []string
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		environments, err = tx.Queries().ReconcileStaleControlConfigWarning(ctx, dbsqlc.ReconcileStaleControlConfigWarningParams{
			WarningRevision: sql.NullString{String: s.warningRevision, Valid: true}, Now: now,
		})
		if err != nil {
			return err
		}
		for _, environmentID := range environments {
			if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "config.warning_superseded",
				ResourceType: "environment", ResourceID: environmentID,
				IdempotencyKey: "config.warning.superseded:" + environmentID + ":" + s.warningRevision,
				Metadata:       map[string]any{"warning_revision": s.warningRevision}}); err != nil {
				return err
			}
		}
		return nil
	})
	return environments, err
}

func (s *ConfigAssignmentService) WarningReconciliationWorker(interval time.Duration) func(context.Context) error {
	if interval <= 0 {
		interval = time.Minute
	}
	return func(ctx context.Context) error {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			finishProgress := workers.BeginIteration(ctx, interval)
			cycleErr := func() error {
				if _, err := s.ReconcileWarning(ctx); err != nil {
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

func (s *ConfigAssignmentService) Clear(ctx context.Context, userID, machineID string, expectedVersion int64) error {
	if userID == "" || machineID == "" || expectedVersion < 1 {
		return ErrAssignmentForbidden
	}
	environmentID, err := s.ownedMachineEnvironment(ctx, userID, machineID)
	if err != nil {
		return err
	}
	var assignment dbsqlc.ControlConfigAssignment
	now := s.clock().UTC()
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		assignment, err = tx.Queries().ClearControlConfigAssignment(ctx, dbsqlc.ClearControlConfigAssignmentParams{EnvironmentID: environmentID, ExpectedVersion: expectedVersion, Now: sql.NullTime{Time: now, Valid: true}})
		if err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigCredentialsForEnvironment(ctx, dbsqlc.RevokeControlConfigCredentialsForEnvironmentParams{EnvironmentID: environmentID, RevokedAt: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryAccessForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryAccessForEnvironmentParams{EnvironmentID: environmentID, Now: now}); err != nil {
			return err
		}
		if _, err = tx.Queries().RevokeControlConfigRepositoryLeasesForEnvironment(ctx, dbsqlc.RevokeControlConfigRepositoryLeasesForEnvironmentParams{EnvironmentID: sql.NullString{String: environmentID, Valid: true}, Now: sql.NullTime{Time: now, Valid: true}}); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: userID, ActorType: audit.ActorUser, EventType: "config.assignment_cleared", ResourceType: "environment", ResourceID: environmentID, IdempotencyKey: "config.assignment.clear:" + environmentID + ":" + strconv.FormatInt(assignment.Version, 10), Metadata: map[string]any{}})
	})
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAssignmentConflict
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *ConfigAssignmentService) ownedMachineEnvironment(ctx context.Context, userID, machineID string) (string, error) {
	machine, err := s.store.Queries().GetUserMachineForUser(ctx, dbsqlc.GetUserMachineForUserParams{ID: machineID, UserID: userID})
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAssignmentForbidden
	}
	if err != nil {
		return "", err
	}
	environment, err := s.store.Queries().GetControlEnvironment(ctx, machine.EnvironmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAssignmentForbidden
	}
	if err != nil {
		return "", err
	}
	if !environment.OwnerUserID.Valid || environment.OwnerUserID.String != userID || environment.DesiredState != "active" {
		return "", ErrAssignmentForbidden
	}
	if scope, selected := workspace.FromContext(ctx); selected {
		if err := workspace.Validate(ctx, s.store, userID, scope); err != nil {
			if errors.Is(err, workspace.ErrForbidden) || errors.Is(err, workspace.ErrInvalid) {
				return "", ErrAssignmentForbidden
			}
			return "", err
		}
		assignment, assignmentErr := s.store.Queries().GetControlConfigAssignment(ctx, machine.EnvironmentID)
		if scope.TeamID == "" {
			if assignmentErr == nil && assignment.AdoptedTeamID.Valid {
				return "", ErrAssignmentForbidden
			}
			if assignmentErr != nil && !errors.Is(assignmentErr, sql.ErrNoRows) {
				return "", assignmentErr
			}
		} else {
			if errors.Is(assignmentErr, sql.ErrNoRows) || assignmentErr == nil && (!assignment.AdoptedTeamID.Valid || assignment.AdoptedTeamID.String != scope.TeamID) {
				return "", ErrAssignmentForbidden
			}
			if assignmentErr != nil {
				return "", assignmentErr
			}
		}
	}
	return machine.EnvironmentID, nil
}

func (s *ConfigAssignmentService) workspaceTeamAllowed(ctx context.Context, userID, teamID string) (bool, error) {
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return true, nil
	}
	if scope.TeamID != teamID {
		return false, nil
	}
	err := workspace.Validate(ctx, s.store, userID, scope)
	if errors.Is(err, workspace.ErrForbidden) || errors.Is(err, workspace.ErrInvalid) {
		return false, nil
	}
	return err == nil, err
}

func workspaceTeamAllowedTx(ctx context.Context, tx *db.Tx, userID, teamID string) (bool, error) {
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return true, nil
	}
	if scope.TeamID != teamID {
		return false, nil
	}
	err := workspace.ValidateTx(ctx, tx, userID, scope)
	if errors.Is(err, workspace.ErrForbidden) || errors.Is(err, workspace.ErrInvalid) {
		return false, nil
	}
	return err == nil, err
}

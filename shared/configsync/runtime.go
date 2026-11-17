package configsync

import "time"

// AssignmentMode is the direction a machine syncs a config repository in.
type AssignmentMode string

const (
	ModePullOnly      AssignmentMode = "pull_only"
	ModePushOnly      AssignmentMode = "push_only"
	ModeBidirectional AssignmentMode = "bidirectional"
)

func (m AssignmentMode) Valid() bool {
	return m == ModePullOnly || m == ModePushOnly || m == ModeBidirectional
}

// PathRule grants a machine an explicit repository-to-machine mapping. Repository data cannot supply or expand the
// destination; the worker verifies native filesystem safety.
type PathRule struct {
	ID             string   `json:"id"`
	Source         string   `json:"source"`
	RepositoryPath string   `json:"repository_path"`
	LocalPath      string   `json:"local_path"`
	Kind           string   `json:"kind"`
	Include        []string `json:"include"`
	Exclude        []string `json:"exclude"`
}

// RepositoryBinding names one repository of an assignment and where it lives.
type RepositoryBinding struct {
	RepositoryID string `json:"repository_id"`
	URL          string `json:"url"`
}

// RuntimePolicy is the sync tuning the server hands a machine. The two exclusion-root lists are filled by the
// machine from its own runtime layout and are never serialized (json:"-").
// shortcut: local-only fields live on the wire type so the machine's descriptor.Policy.* call sites stay one type, split if the server ever needs its own.
type RuntimePolicy struct {
	Format                        string        `json:"format"`
	Revision                      string        `json:"revision"`
	ManifestContract              string        `json:"manifest_contract"`
	ManifestMaxBytes              int           `json:"manifest_max_bytes"`
	ManifestMaxLines              int           `json:"manifest_max_lines"`
	ManifestMaxPatternBytes       int           `json:"manifest_max_pattern_bytes"`
	MaxFileBytes                  int64         `json:"max_file_bytes"`
	MaxBatchBytes                 int64         `json:"max_batch_bytes"`
	Debounce                      time.Duration `json:"debounce"`
	MinimumPushInterval           time.Duration `json:"minimum_push_interval"`
	MaximumDirtyDelay             time.Duration `json:"maximum_dirty_delay"`
	RemotePollInterval            time.Duration `json:"remote_poll_interval"`
	RetryLimit                    int           `json:"retry_limit"`
	ShutdownFlushTimeout          time.Duration `json:"shutdown_flush_timeout"`
	SummaryLimit                  int           `json:"summary_limit"`
	AbsoluteRuntimeExclusionRoots []string      `json:"-"`
	RuntimeExclusionRoots         []string      `json:"-"`
}

// RuntimeDescriptor is what the server tells one machine to sync (proof-authenticated request, tolerant response).
type RuntimeDescriptor struct {
	AssignmentVersion     int64               `json:"assignment_version"`
	ConfigurationRevision string              `json:"configuration_revision"`
	PathRules             []PathRule          `json:"path_rules"`
	RepositoryBindings    []RepositoryBinding `json:"repository_bindings"`
	WriteMode             string              `json:"write_mode"`
	Mode                  AssignmentMode      `json:"mode"`
	RepositoryID          string              `json:"repository_id"`
	PullRepositoryID      string              `json:"pull_repository_id,omitempty"`
	PushRepositoryID      string              `json:"push_repository_id,omitempty"`
	AutomaticUpdates      bool                `json:"automatic_updates"`
	ApprovedPullRevision  string              `json:"approved_pull_revision,omitempty"`
	AssignmentID          string              `json:"assignment_id"`
	EnvironmentID         string              `json:"environment_id"`
	MachineID             string              `json:"machine_id"`
	// MachineAlias selects the shared file's [machine.<name>] section.
	MachineAlias           string        `json:"machine_alias"`
	InstallationGeneration int64         `json:"installation_generation"`
	SyncRevisionFloor      int64         `json:"sync_revision_floor"`
	WarningRevision        string        `json:"warning_revision"`
	Policy                 RuntimePolicy `json:"policy"`
}

// Assignment is a config assignment as the control plane serves it to the account (user session). Pull is omitted
// for a push-only assignment, and an unset warning revision is omitted.
type Assignment struct {
	ID                    string              `json:"id"`
	MachineID             string              `json:"machine_id"`
	EnvironmentID         string              `json:"environment_id"`
	PullRepositoryID      *string             `json:"pull_repository_id,omitempty"`
	PushRepositoryID      *string             `json:"push_repository_id,omitempty"`
	AutomaticUpdates      bool                `json:"automatic_updates"`
	ConsentState          string              `json:"consent_state"`
	Mode                  string              `json:"mode"`
	WarningRevision       *string             `json:"warning_revision,omitempty"`
	ConfigurationRevision string              `json:"configuration_revision"`
	Version               int64               `json:"version"`
	PathRules             []PathRule          `json:"path_rules"`
	RepositoryBindings    []RepositoryBinding `json:"repository_bindings"`
	Source                string              `json:"source"`
}

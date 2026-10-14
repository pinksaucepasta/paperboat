// Package releaseplan contains the deterministic release publication and
// deployment policy boundary. It does not replace the host updater journal:
// the updater owns binary activation, while this package authenticates the
// artifact set and supplies bounded rollout/rollback inputs.
package releaseplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releasepolicy"
)

const (
	ManifestSchema = "paperboat.release-artifacts/v1"
	PlanSchema     = releasepolicy.PlanSchema
	StateSchema    = "paperboat.release-deployment-state/v1"

	MaxArtifactBytes       int64  = 512 << 20
	MaxManifestBytes       int64  = 1 << 20
	MaxPlanBytes           int64  = 1 << 20
	MaxStateBytes          int64  = 256 << 10
	MaxCohortDelaySeconds  uint32 = releasepolicy.MaxCohortDelaySeconds
	MaxRoutineDeferralSec  uint32 = releasepolicy.MaxRoutineDeferralSec
	MaxSecurityDeferralSec uint32 = releasepolicy.MaxSecurityDeferralSec
	MaxCriticalDeferralSec uint32 = releasepolicy.MaxCriticalDeferralSec
	DeferralSchema         string = releasepolicy.DeferralSchema
)

var (
	ErrInvalidManifest = errors.New("invalid release artifact manifest")
	ErrInvalidPlan     = releasepolicy.ErrInvalidPolicy
	ErrInvalidState    = errors.New("invalid release deployment state")
	ErrInvalidEvent    = errors.New("invalid release deployment event")
	ErrUnsafePath      = errors.New("unsafe release path")
)

var (
	commitPattern    = regexp.MustCompile(`^[a-f0-9]{40,64}$`)
	toolchainPattern = regexp.MustCompile(`^go[0-9]+\.[0-9]+\.[0-9]+$`)
)

type safeFileFailure struct {
	message string
	cause   error
}

func (e safeFileFailure) Error() string { return e.message }

func (e safeFileFailure) Unwrap() error { return e.cause }

func fileFailure(message string, causes ...error) error {
	cause := errors.Join(causes...)
	if cause == nil {
		return nil
	}
	return safeFileFailure{message: message, cause: cause}
}

type artifactSpec struct {
	Platform     string
	Architecture string
	Format       string
	Name         string
}

var artifactSpecs = []artifactSpec{
	{Platform: "darwin", Architecture: "arm64", Format: "pkg", Name: "pb-darwin-arm64.pkg"},
	{Platform: "linux", Architecture: "amd64", Format: "elf", Name: "pb-linux-amd64"},
	{Platform: "linux", Architecture: "arm64", Format: "elf", Name: "pb-linux-arm64"},
	{Platform: "windows", Architecture: "amd64", Format: "pe", Name: "pb-windows-amd64.exe"},
	{Platform: "windows", Architecture: "arm64", Format: "pe", Name: "pb-windows-arm64.exe"},
}

// Artifact is the immutable identity of one native release asset. Its fields
// intentionally mirror the signed TUF target and release-index coordinates.
type Artifact struct {
	Name         string `json:"name"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	Format       string `json:"format"`
	Length       int64  `json:"length"`
	SHA256       string `json:"sha256"`
}

// Manifest is reproducible: it contains no wall-clock field or local path.
// The manifest digest is referenced by the deployment plan and can therefore
// be checked against the exact artifact set before TUF publication.
type Manifest struct {
	Schema       string     `json:"schema"`
	Version      string     `json:"version"`
	SourceCommit string     `json:"source_commit"`
	Toolchain    string     `json:"toolchain"`
	Artifacts    []Artifact `json:"artifacts"`
}

func (m Manifest) Validate() error {
	if m.Schema != ManifestSchema || !releasepolicy.IsVersion(m.Version) || !commitPattern.MatchString(m.SourceCommit) || !toolchainPattern.MatchString(m.Toolchain) || len(m.Artifacts) == 0 || len(m.Artifacts) > len(artifactSpecs) {
		return ErrInvalidManifest
	}
	previous := -1
	for _, artifact := range m.Artifacts {
		index := -1
		for candidate, spec := range artifactSpecs {
			if artifact.Name == spec.Name {
				index = candidate
				break
			}
		}
		if index <= previous {
			return ErrInvalidManifest
		}
		spec := artifactSpecs[index]
		if artifact.Platform != spec.Platform || artifact.Architecture != spec.Architecture || artifact.Format != spec.Format || artifact.Length < 1 || artifact.Length > MaxArtifactBytes || !releasepolicy.IsDigest(artifact.SHA256) {
			return ErrInvalidManifest
		}
		previous = index
	}

	return nil
}

func (m Manifest) Bytes() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func (m Manifest) SHA256() (string, error) {
	body, err := m.Bytes()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// BuildManifest reads a nonempty canonical subset of supported release files. It rejects
// symlinks, directories, extra files, and a file that changes while hashing.
func BuildManifest(version, sourceCommit, toolchain, artifactDirectory string) (Manifest, error) {
	if !releasepolicy.IsVersion(version) || !commitPattern.MatchString(sourceCommit) || !toolchainPattern.MatchString(toolchain) {
		return Manifest{}, ErrInvalidManifest
	}
	directoryIsClean, directoryErr := absoluteCleanDirectory(artifactDirectory)
	if directoryErr != nil {
		return Manifest{}, fileFailure("release artifact directory could not be inspected", ErrInvalidManifest, directoryErr)
	}
	if !directoryIsClean {
		return Manifest{}, ErrInvalidManifest
	}
	entries, err := os.ReadDir(artifactDirectory)
	if err != nil {
		return Manifest{}, fileFailure("release artifact directory could not be read", err)
	}
	if len(entries) == 0 || len(entries) > len(artifactSpecs) {
		return Manifest{}, ErrInvalidManifest
	}
	artifacts := make([]Artifact, 0, len(artifactSpecs))
	for _, spec := range artifactSpecs {
		path := filepath.Join(artifactDirectory, spec.Name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Manifest{}, fileFailure("release artifact could not be inspected", ErrInvalidManifest, err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > MaxArtifactBytes {
			return Manifest{}, ErrInvalidManifest
		}
		length, digest, err := hashRegularFile(path, MaxArtifactBytes)
		if err != nil {
			return Manifest{}, fileFailure("release artifact could not be hashed", ErrInvalidManifest, err)
		}
		if length != info.Size() {
			return Manifest{}, ErrInvalidManifest
		}
		artifacts = append(artifacts, Artifact{Name: spec.Name, Platform: spec.Platform, Architecture: spec.Architecture, Format: spec.Format, Length: length, SHA256: digest})
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return Manifest{}, ErrInvalidManifest
		}
		known := false
		for _, spec := range artifactSpecs {
			if entry.Name() == spec.Name {
				known = true
				break
			}
		}
		if !known {
			return Manifest{}, ErrInvalidManifest
		}
	}
	return NewManifest(version, sourceCommit, toolchain, artifacts)
}

// NewManifest validates a manifest assembled from already-hashed release
// artifacts. The TUF publisher uses this when the artifact staging directory
// also contains platform qualification evidence that is intentionally not a
// release target.
func NewManifest(version, sourceCommit, toolchain string, artifacts []Artifact) (Manifest, error) {
	manifest := Manifest{Schema: ManifestSchema, Version: version, SourceCommit: sourceCommit, Toolchain: toolchain, Artifacts: append([]Artifact(nil), artifacts...)}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func VerifyManifest(manifest Manifest, artifactDirectory string) error {
	if err := manifest.Validate(); err != nil {
		return ErrInvalidManifest
	}
	directoryIsClean, directoryErr := absoluteCleanDirectory(artifactDirectory)
	if directoryErr != nil {
		return fileFailure("release artifact directory could not be inspected", ErrInvalidManifest, directoryErr)
	}
	if !directoryIsClean {
		return ErrInvalidManifest
	}
	entries, err := os.ReadDir(artifactDirectory)
	if err != nil {
		return fileFailure("release artifact directory could not be read", ErrInvalidManifest, err)
	}
	if len(entries) != len(manifest.Artifacts) {
		return ErrInvalidManifest
	}
	for _, artifact := range manifest.Artifacts {
		path := filepath.Join(artifactDirectory, artifact.Name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return fileFailure("release artifact could not be inspected", ErrInvalidManifest, statErr)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != artifact.Length {
			return ErrInvalidManifest
		}
		length, digest, readErr := hashRegularFile(path, MaxArtifactBytes)
		if readErr != nil {
			return fileFailure("release artifact could not be verified", ErrInvalidManifest, readErr)
		}
		if length != artifact.Length || digest != artifact.SHA256 {
			return ErrInvalidManifest
		}
	}
	return nil
}

func LoadManifest(path string) (Manifest, error) {
	body, err := readBoundedJSON(path, MaxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := decodeStrict(body, &manifest); err != nil || manifest.Validate() != nil {
		return Manifest{}, ErrInvalidManifest
	}
	return manifest, nil
}

type Cohort = releasepolicy.Cohort
type CanaryPolicy = releasepolicy.CanaryPolicy
type ActivationPolicy = releasepolicy.ActivationPolicy
type SecurityDeferral = releasepolicy.SecurityDeferral
type RollbackPolicy = releasepolicy.RollbackPolicy
type Plan = releasepolicy.Plan
type DeferralRequest = releasepolicy.DeferralRequest
type Deferral = releasepolicy.Deferral

// ValidatePlanAgainstManifest verifies both the signed policy identity and
// exact platform coverage of the selected native artifact set.
func ValidatePlanAgainstManifest(plan Plan, manifest Manifest) error {
	if manifest.Validate() != nil {
		return ErrInvalidPlan
	}
	digest, err := manifest.SHA256()
	if err != nil || plan.ValidateAgainst(manifest.Version, digest) != nil {
		return ErrInvalidPlan
	}
	selected := make(map[string]bool, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		selected[artifact.Platform+"/"+artifact.Architecture] = false
	}
	for _, cohort := range plan.Cohorts {
		key := cohort.Platform + "/" + cohort.Architecture
		if _, ok := selected[key]; !ok {
			return ErrInvalidPlan
		}
		selected[key] = true
	}
	for _, covered := range selected {
		if !covered {
			return ErrInvalidPlan
		}
	}

	return nil
}

func LoadPlan(path string) (Plan, error) {
	body, err := readBoundedJSON(path, MaxPlanBytes)
	if err != nil {
		return Plan{}, err
	}
	var plan Plan
	if err := decodeStrict(body, &plan); err != nil || plan.Validate() != nil {
		return Plan{}, ErrInvalidPlan
	}
	return plan, nil
}

func DefaultPlan(manifest Manifest, policyRevision uint64, severity, seed string) (Plan, error) {
	digest, err := manifest.SHA256()
	if err != nil || !releasepolicy.IsIdentifier(seed) || policyRevision == 0 {
		return Plan{}, ErrInvalidPlan
	}
	targets := make([]releasepolicy.PlatformTarget, 0, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		targets = append(targets, releasepolicy.PlatformTarget{Platform: artifact.Platform, Architecture: artifact.Architecture})
	}
	plan, err := releasepolicy.Default(manifest.Version, digest, policyRevision, severity, seed, targets)
	if err != nil {
		return Plan{}, ErrInvalidPlan
	}
	if err := ValidatePlanAgainstManifest(plan, manifest); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

type DeploymentState string

const (
	StateScheduled           DeploymentState = "scheduled"
	StateDownloading         DeploymentState = "downloading"
	StateCandidateValidating DeploymentState = "candidate_validating"
	StateCandidateReady      DeploymentState = "candidate_ready"
	StateDraining            DeploymentState = "draining"
	StateActivating          DeploymentState = "activating"
	StateStability           DeploymentState = "stability"
	StateRollingBack         DeploymentState = "rolling_back"
	StateCommitted           DeploymentState = "committed"
	StateRolledBack          DeploymentState = "rolled_back"
	StateQuarantined         DeploymentState = "quarantined"
	StateBlocked             DeploymentState = "blocked"
)

type State struct {
	Schema          string          `json:"schema"`
	TransactionID   string          `json:"transaction_id"`
	Version         string          `json:"version"`
	ManifestSHA256  string          `json:"manifest_sha256"`
	PreviousVersion string          `json:"previous_version"`
	State           DeploymentState `json:"state"`
	Failure         string          `json:"failure,omitempty"`
	Quarantined     bool            `json:"quarantined"`
	Revoked         bool            `json:"revoked"`
	UpdatedAt       time.Time       `json:"updated_at"`
	QuarantineUntil *time.Time      `json:"quarantine_until,omitempty"`
}

func NewState(plan Plan, transactionID, previousVersion string, now time.Time) (State, error) {
	if plan.Validate() != nil || !releasepolicy.IsIdentifier(transactionID) || !releasepolicy.IsVersion(previousVersion) || now.IsZero() {
		return State{}, ErrInvalidState
	}
	return State{Schema: StateSchema, TransactionID: transactionID, Version: plan.Version, ManifestSHA256: plan.ManifestSHA256, PreviousVersion: previousVersion, State: StateScheduled, UpdatedAt: now.UTC()}, nil
}

type Event string

const (
	EventDownloadStarted     Event = "download_started"
	EventCandidateValidating Event = "candidate_validating"
	EventCandidateReady      Event = "candidate_ready"
	EventCanaryPassed        Event = "canary_passed"
	EventCanaryFailed        Event = "canary_failed"
	EventDrainCompleted      Event = "drain_completed"
	EventDrainFailed         Event = "drain_failed"
	EventActivationStarted   Event = "activation_started"
	EventActivationPassed    Event = "activation_passed"
	EventActivationFailed    Event = "activation_failed"
	EventStabilityPassed     Event = "stability_passed"
	EventStabilityFailed     Event = "stability_failed"
	EventRollbackStarted     Event = "rollback_started"
	EventRollbackPassed      Event = "rollback_passed"
	EventRollbackFailed      Event = "rollback_failed"
	EventRevoke              Event = "revoke"
)

// Advance is the release coordinator state machine. All failure paths after
// activation enter rollback; failed rollback is blocked and remains
// quarantined, so an operator cannot accidentally retry a bad artifact.
func Advance(state State, event Event, reason string, now time.Time, quarantineSeconds uint32) (State, error) {
	if err := state.Validate(); err != nil || now.IsZero() || len(reason) > 256 || strings.ContainsAny(reason, "\x00\r\n") {
		return State{}, ErrInvalidState
	}
	next := state
	next.UpdatedAt = now.UTC()
	if reason != "" {
		next.Failure = reason
	}
	transition := func(value DeploymentState) { next.State = value }
	switch event {
	case EventDownloadStarted:
		if state.State != StateScheduled {
			return State{}, ErrInvalidEvent
		}
		transition(StateDownloading)
	case EventCandidateReady:
		if state.State != StateCandidateValidating {
			return State{}, ErrInvalidEvent
		}
		transition(StateCandidateReady)
	case EventCandidateValidating:
		if state.State != StateDownloading {
			return State{}, ErrInvalidEvent
		}
		transition(StateCandidateValidating)
	case EventCanaryPassed:
		if state.State != StateCandidateReady {
			return State{}, ErrInvalidEvent
		}
		transition(StateDraining)
	case EventCanaryFailed, EventDrainFailed:
		if event == EventCanaryFailed && state.State != StateCandidateReady || event == EventDrainFailed && state.State != StateDraining {
			return State{}, ErrInvalidEvent
		}
		transition(StateQuarantined)
		next.Quarantined = true
	case EventDrainCompleted:
		if state.State != StateDraining {
			return State{}, ErrInvalidEvent
		}
		transition(StateActivating)
	case EventActivationStarted:
		if state.State != StateActivating {
			return State{}, ErrInvalidEvent
		}
		transition(StateActivating)
	case EventActivationPassed:
		if state.State != StateActivating {
			return State{}, ErrInvalidEvent
		}
		transition(StateStability)
	case EventActivationFailed, EventStabilityFailed:
		if event == EventActivationFailed && state.State != StateActivating || event == EventStabilityFailed && state.State != StateStability {
			return State{}, ErrInvalidEvent
		}
		transition(StateRollingBack)
	case EventStabilityPassed:
		if state.State != StateStability {
			return State{}, ErrInvalidEvent
		}
		transition(StateCommitted)
	case EventRollbackStarted:
		if state.State != StateRollingBack {
			return State{}, ErrInvalidEvent
		}
		transition(StateRollingBack)
	case EventRollbackPassed:
		if state.State != StateRollingBack {
			return State{}, ErrInvalidEvent
		}
		transition(StateRolledBack)
		next.Quarantined = true
	case EventRollbackFailed:
		if state.State != StateRollingBack {
			return State{}, ErrInvalidEvent
		}
		transition(StateBlocked)
		next.Quarantined = true
	case EventRevoke:
		if state.State == StateCommitted {
			return State{}, ErrInvalidEvent
		}
		transition(StateQuarantined)
		next.Quarantined, next.Revoked = true, true
	default:
		return State{}, ErrInvalidEvent
	}
	if next.Quarantined {
		if quarantineSeconds == 0 || quarantineSeconds > 30*24*60*60 {
			return State{}, ErrInvalidState
		}
		expires := next.UpdatedAt.Add(time.Duration(quarantineSeconds) * time.Second)
		next.QuarantineUntil = &expires
	}
	return next, nil
}

func (s State) Validate() error {
	if s.Schema != StateSchema || !releasepolicy.IsIdentifier(s.TransactionID) || !releasepolicy.IsVersion(s.Version) || !releasepolicy.IsDigest(s.ManifestSHA256) || !releasepolicy.IsVersion(s.PreviousVersion) || !knownState(s.State) || s.UpdatedAt.IsZero() || len(s.Failure) > 256 || strings.ContainsAny(s.Failure, "\x00\r\n") {
		return ErrInvalidState
	}
	if s.Quarantined && s.QuarantineUntil == nil || !s.Quarantined && s.QuarantineUntil != nil || s.QuarantineUntil != nil && !s.QuarantineUntil.After(s.UpdatedAt) {
		return ErrInvalidState
	}
	if s.Revoked && !s.Quarantined {
		return ErrInvalidState
	}
	return nil
}

func (s State) Bytes() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func LoadState(path string) (State, error) {
	body, err := readBoundedJSON(path, MaxStateBytes)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := decodeStrict(body, &state); err != nil || state.Validate() != nil {
		return State{}, ErrInvalidState
	}
	return state, nil
}

func WriteFile(path string, body []byte, max int64) (resultErr error) {
	if !absoluteCleanPath(path) || len(body) == 0 || int64(len(body)) > max {
		return ErrUnsafePath
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fileFailure("release output directory could not be created", err)
	}
	//paperboat:allow-source-policy atomic-replacement owner=release-plan reason=same-directory-release-metadata-staging
	temporary, err := os.CreateTemp(directory, ".paperboat-release-")
	if err != nil {
		return fileFailure("release metadata staging file could not be created", err)
	}
	temporaryPath := temporary.Name()
	closed := false
	committed := false
	defer func() {
		var cleanupErrs []error
		if !closed {
			if err := temporary.Close(); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if !committed {
			if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		if len(cleanupErrs) == 0 {
			return
		}
		cleanupCause := errors.Join(cleanupErrs...)
		if resultErr == nil {
			resultErr = fileFailure("release metadata cleanup could not be confirmed", cleanupCause)
			return
		}
		resultErr = fileFailure("release metadata write failed and cleanup was incomplete", resultErr, cleanupCause)
	}()
	if err := temporary.Chmod(0o644); err != nil {
		return fileFailure("release metadata permissions could not be set", err)
	}
	written, err := temporary.Write(body)
	if err != nil {
		return fileFailure("release metadata could not be written", err)
	}
	if written != len(body) {
		return fileFailure("release metadata could not be written", io.ErrShortWrite)
	}
	if err := temporary.Sync(); err != nil {
		return fileFailure("release metadata could not be synchronized", err)
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return fileFailure("release metadata staging file could not be closed", err)
	}
	closed = true
	//paperboat:allow-source-policy atomic-replacement owner=release-plan reason=same-directory-synced-release-metadata-staging
	if err := os.Rename(temporaryPath, path); err != nil {
		return fileFailure("release metadata could not be committed", err)
	}
	committed = true
	directoryFile, err := os.Open(directory)
	if err != nil {
		return fileFailure("release metadata was replaced but durability could not be confirmed", err)
	}
	syncErr := directoryFile.Sync()
	closeErr := directoryFile.Close()
	if syncErr != nil || closeErr != nil {
		return fileFailure("release metadata was replaced but durability could not be confirmed", syncErr, closeErr)
	}
	return nil
}

func readBoundedJSON(path string, max int64) ([]byte, error) {
	if !absoluteCleanPath(path) {
		return nil, ErrUnsafePath
	}
	return readRegularFile(path, max)
}

func readRegularFile(path string, max int64) ([]byte, error) {
	file, info, err := openRegularFile(path, max)
	if err != nil {
		return nil, fileFailure("release file could not be opened safely", ErrUnsafePath, err)
	}
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	changed, changeErr := regularFileChanged(file, info)
	closeErr := file.Close()
	if err != nil || changeErr != nil || closeErr != nil {
		return nil, fileFailure("release file could not be read safely", ErrUnsafePath, err, changeErr, closeErr)
	}
	if int64(len(body)) != info.Size() || int64(len(body)) > max || changed {
		return nil, ErrUnsafePath
	}
	return body, nil
}

func hashRegularFile(path string, max int64) (int64, string, error) {
	file, info, err := openRegularFile(path, max)
	if err != nil {
		return 0, "", fileFailure("release artifact could not be opened safely", ErrInvalidManifest, err)
	}
	hash := sha256.New()
	length, err := io.Copy(hash, io.LimitReader(file, max+1))
	changed, changeErr := regularFileChanged(file, info)
	closeErr := file.Close()
	if err != nil || changeErr != nil || closeErr != nil {
		return 0, "", fileFailure("release artifact could not be hashed safely", ErrInvalidManifest, err, changeErr, closeErr)
	}
	if length != info.Size() || length > max || changed {
		return 0, "", ErrInvalidManifest
	}
	return length, hex.EncodeToString(hash.Sum(nil)), nil
}

func openRegularFile(path string, max int64) (*os.File, os.FileInfo, error) {
	if !absoluteCleanPath(path) || max < 1 {
		return nil, nil, ErrUnsafePath
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, fileFailure("release file could not be inspected", ErrUnsafePath, err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode()&os.ModeSymlink != 0 || pathInfo.Size() < 1 || pathInfo.Size() > max {
		return nil, nil, ErrUnsafePath
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fileFailure("release file could not be opened", err)
	}
	fileInfo, statErr := file.Stat()
	if statErr != nil || fileInfo == nil || !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) || fileInfo.Size() < 1 || fileInfo.Size() > max {
		closeErr := file.Close()
		return nil, nil, fileFailure("opened release file could not be verified", ErrUnsafePath, statErr, closeErr)
	}
	return file, fileInfo, nil
}

func regularFileChanged(file *os.File, before os.FileInfo) (bool, error) {
	after, err := file.Stat()
	if err != nil {
		return true, err
	}
	return !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()), nil
}

func decodeStrict(body []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func absoluteCleanDirectory(path string) (bool, error) {
	if !absoluteCleanPath(path) {
		return false, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0, nil
}

func absoluteCleanPath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func knownState(state DeploymentState) bool {
	switch state {
	case StateScheduled, StateDownloading, StateCandidateValidating, StateCandidateReady, StateDraining, StateActivating, StateStability, StateRollingBack, StateCommitted, StateRolledBack, StateQuarantined, StateBlocked:
		return true
	default:
		return false
	}
}

// ArtifactSpecs returns a copy for release workflow tests and callers that
// need to enumerate supported assets without inventing a new target.
func ArtifactSpecs() []Artifact {
	result := make([]Artifact, len(artifactSpecs))
	for index, spec := range artifactSpecs {
		result[index] = Artifact{Name: spec.Name, Platform: spec.Platform, Architecture: spec.Architecture, Format: spec.Format}
	}
	return result
}

func (m Manifest) Artifact(name string) (Artifact, bool) {
	for _, artifact := range m.Artifacts {
		if artifact.Name == name {
			return artifact, true
		}
	}
	return Artifact{}, false
}

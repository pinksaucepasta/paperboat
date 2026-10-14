// Package workerupdate stages and approves verified runtime artifacts.
// Native installation owns activation, service lifetime, and rollback.
package workerupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/binarytarget"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesignature"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

const maxRuntimeBytes int64 = 512 << 20

var (
	ErrInvalidConfig  = errors.New("invalid worker update configuration")
	ErrInvalidRelease = errors.New("invalid worker release")
	ErrBlocked        = errors.New("worker updates require recovery")
	ErrQuarantined    = errors.New("worker release is quarantined")
	ErrReleaseRevoked = errors.New("worker release is revoked")
	ErrUnsafeStorage  = errors.New("unsafe worker release storage")
)

// Release describes a verified signed candidate or a typed installed baseline. Fetch transports
// its bytes but does not establish its identity: the updater independently
// checks this exact version, hash, length, platform, and architecture before
// making it executable.
type Release struct {
	// LocalSource binds an administrator-approved installed baseline. It is never an update candidate or signed manifest.
	LocalSource       *installsource.Source `json:",omitempty"`
	Version           string
	SHA256            string
	Length            int64
	Platform          string
	Architecture      string
	ManifestSHA256    string
	CanaryPath        string
	CanaryStatus      int
	CanarySamples     uint16
	CanaryTimeout     time.Duration
	DrainTimeout      time.Duration
	StabilityWindow   time.Duration
	StabilityInterval time.Duration
	RollbackTimeout   time.Duration
	CLISHA256         string
	CLILength         int64
	CLIPlatform       string
	CLIArchitecture   string
	HostdAPIMin       uint16
	HostdAPIMax       uint16
	RuntimeAPIMin     uint16
	RuntimeAPIMax     uint16
	// Supervisor targets are present in every signed release index. They are
	// kept separate from the worker/CLI targets because replacing them is a
	// supervisor-class maintenance operation.
	Hostd                 ComponentTarget
	Updater               ComponentTarget
	Launcher              ComponentTarget
	SupervisorMaintenance bool
	// OwnerMaintenanceGraceSeconds is the verified policy's ceiling for a
	// local announced maintenance window, not a rollout deferral grant.
	OwnerMaintenanceGraceSeconds uint32
}

// ComponentTarget is the immutable target identity copied from the verified
// release index. It is never populated from a local path or a caller.
type ComponentTarget struct {
	SHA256       string
	Length       int64
	Platform     string
	Architecture string
}

// Resolver returns the newest signed and cohort-eligible release. found=false
// means the signed index contains no newer eligible release.
type Resolver func(context.Context) (release Release, found bool, err error)

// Fetcher returns the signed release artifact. The returned stream is treated
// as untrusted until Manager has copied, fsynced, hash-checked, size-checked,
// and native-format-checked it in its root-owned staging path.
type Fetcher interface {
	Fetch(context.Context, Release) (io.ReadCloser, error)
	// AuthorizeRecovery rechecks the current signed rollback policy for an
	// already authenticated local executable. Artifact identity is verified
	// independently from the local bytes because Darwin signs a package whose
	// installed payload has a different digest.
	AuthorizeRecovery(context.Context, string, string, string) error
}

// ComponentFetcher is implemented by the TUF source. The CLI target is
// resolved from the same already verified release index as the runtime.
type ComponentFetcher interface {
	FetchComponent(context.Context, Release, string) (io.ReadCloser, error)
}

// Hostd reports the native owner’s active runtime fence.
type Hostd interface {
	Active(context.Context) (hostdproto.Status, error)
}

// HealthChecker validates the newly active worker during the monitoring hold.
// Implementations check bounded worker health, control-plane sync, relay
// readiness, and resource limits without inspecting terminal data.
type HealthChecker interface {
	Check(context.Context, hostdproto.Status, Release) error
}

// NativeVerifier is the platform-native release gate. It runs only after TUF
// digest, length, and binary-format validation has completed in a private
// staging path and before the candidate is made executable by hostd.
type NativeVerifier interface {
	Verify(context.Context, string, string, string) error
}

type Config struct {
	// ApplyPrepared hands verified bytes to the native installation owner. That
	// owner alone replaces services, publishes Source, and recovers failures.
	ApplyPrepared func(context.Context, PreparedActivation) (installsource.Source, error)
	// FinalizePrepared owns retryable post-commit maintenance such as manuals.
	// Failure keeps staging receipts without rolling back committed services.
	FinalizePrepared func(context.Context, installsource.Source) error
	// RecoveryExecutable is the verified previous helper retained while the
	// canonical slot is replaced by the native installation owner.
	RecoveryExecutable string
	// ManualActivation retains explicit user intent when terminal admission defers activation.
	ManualActivation bool
	// ForceActivation explicitly permits interrupting protected sessions. Approval alone does not.
	ForceActivation bool
	StatePath       string
	Binary          string
	BinaryStaged    string
	Active          Release
	OwnerUID        int
	OwnerGID        int
	Fetcher         Fetcher
	Hostd           Hostd
	Health          HealthChecker
	Gate            ActivationGate
	Events          EventSink
	NativeVerifier  NativeVerifier
	// ExtractPackage is the macOS package extraction boundary. A pkg is never
	// renamed into an executable slot; the installer returns the executable
	// installed by the verified package so it can be staged atomically.
	ExtractPackage  func(context.Context, string, string) (string, error)
	MonitorWindow   time.Duration
	HealthInterval  time.Duration
	CanaryTimeout   time.Duration
	DrainTimeout    time.Duration
	RollbackTimeout time.Duration
	Now             func() time.Time
	WriteJournal    func(string, updateflow.Journal, int, int) error
}

// Manager owns one verified staging/approval transaction. Native installation
// alone owns canonical and rollback payloads; failed candidates remain staged.
type Manager struct {
	mu            sync.Mutex
	config        Config
	active        Release
	activeVersion atomic.Value
}

type Result struct {
	Version string
	Updated bool
}

func New(config Config) (*Manager, error) {
	if config.MonitorWindow == 0 {
		config.MonitorWindow = 10 * time.Minute
	}
	if config.HealthInterval == 0 {
		config.HealthInterval = time.Second
	}
	if config.CanaryTimeout == 0 {
		config.CanaryTimeout = 30 * time.Second
	}
	if config.DrainTimeout == 0 {
		config.DrainTimeout = 30 * time.Second
	}
	if config.RollbackTimeout == 0 {
		config.RollbackTimeout = 2 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.WriteJournal == nil {
		config.WriteJournal = updateflow.Write
	}
	if config.NativeVerifier == nil {
		config.NativeVerifier = nativesignature.New(nil)
	}
	if config.ExtractPackage == nil && runtime.GOOS == "darwin" {
		config.ExtractPackage = ExtractDarwinPackage
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	manager := &Manager{config: config, active: config.Active}
	manager.activeVersion.Store(config.Active.Version)
	return manager, nil
}

// Check resolves a signed cohort-eligible target and applies it through the
// verified native activation callback. This is the sole function that an automatic
// update scheduler may call.
func (m *Manager) Check(ctx context.Context, resolve Resolver) (Result, error) {
	if resolve == nil {
		return Result{}, ErrInvalidConfig
	}
	release, found, err := resolve(ctx)
	if err != nil || !found {
		return Result{Version: m.ActiveVersion()}, err
	}
	return Result{Version: release.Version}, nil
}

func (m *Manager) ActiveVersion() string {
	if m == nil {
		return ""
	}
	version := m.activeVersion.Load()
	if version == nil {
		return ""
	}
	return version.(string)
}

func (m *Manager) Activate(ctx context.Context, release Release) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Version: m.ActiveVersion()}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{Version: m.active.Version}, err
	}
	if err := m.recoverLocked(ctx); err != nil {
		return Result{Version: m.active.Version}, err
	}
	if err := validateRelease(release); err != nil || release.LocalSource != nil {
		return Result{Version: m.active.Version}, ErrInvalidRelease
	}
	comparison := 1
	if m.active.LocalSource == nil || m.active.LocalSource.Distribution == installsource.Official {
		comparison = compareVersion(release.Version, m.active.Version)
	}
	if comparison == 0 {
		return Result{Version: m.active.Version}, nil
	}
	if comparison < 0 {
		return Result{Version: m.active.Version}, ErrInvalidRelease
	}
	if quarantine, err := m.quarantinedLocked(); err != nil {
		return Result{Version: m.active.Version}, err
	} else if quarantine == release.Version {
		return Result{Version: m.active.Version}, ErrQuarantined
	}

	prepared, err := PreparedCandidateForRelease(release)
	if err != nil {
		return Result{Version: m.active.Version}, err
	}
	journal, err := updateflow.Load(m.config.StatePath)
	if err != nil {
		return Result{Version: m.active.Version}, err
	}
	if journal.Stage != updateflow.StageAwaitingApproval || journal.CandidateID != prepared.ID || journal.ApprovedCandidateID != prepared.ID {
		return Result{Version: m.active.Version}, ErrApprovalRequired
	}
	if err = m.verifyPrepared(ctx, journal); err != nil {
		return Result{Version: m.active.Version}, err
	}
	localRelease := release
	localRelease.SHA256, localRelease.Length = journal.CandidateDigest, journal.CandidateLength
	return m.applyPrepared(ctx, journal, release, localRelease, prepared)
}

// Recover reconciles only staging state. Native installation owns every
// service and payload transition, including interrupted activation recovery.
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recoverLocked(ctx)
}

func (m *Manager) recoverLocked(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	journal, err := updateflow.Load(m.config.StatePath)
	if journalMissingOnly(err) {
		return m.write(m.newJournal())
	}
	if err != nil {
		return errors.Join(ErrBlocked, err)
	}
	if journal.Stage == updateflow.StageIdle && m.active.LocalSource != nil {
		if journal.ActiveVersion != m.active.Version || journal.ActiveDigest != m.active.SHA256 || journal.ActiveLength != m.active.Length || journal.ActiveSource == nil || *journal.ActiveSource != *m.active.LocalSource {
			return m.write(m.newJournal())
		}
	}
	if !samePreparedBaseline(journal, m.active) {
		return ErrBlocked
	}
	switch journal.Stage {
	case updateflow.StageIdle, updateflow.StageAwaitingApproval:
		return nil
	case updateflow.StageChecking:
		if err := m.removeStaged(); err != nil {
			return err
		}
		return m.write(m.idleJournal(journal, updateflow.FailureCandidate, ""))
	default:
		// Native activation owns service/files recovery; staging never repairs old
		// cutover records or restores a canonical slot from an inferred baseline.
		return ErrBlocked
	}
}

func (m *Manager) validActiveRuntime(status hostdproto.Status, release Release) bool {
	return status.State == hostdproto.StateActive && status.WorkerID == workerID(release.Version) && status.Epoch > 0 && status.APIVersion >= release.HostdAPIMin && status.APIVersion <= release.HostdAPIMax
}

func (m *Manager) setActive(release Release) {
	m.active = release
	m.activeVersion.Store(release.Version)
}

func releaseDuration(signed, fallback time.Duration) time.Duration {
	if signed > 0 {
		return signed
	}
	return fallback
}

// Match the native Windows observation allowance: at least one second for
// completion, no more than thirty seconds regardless of the probe interval.
func stabilityCompletionMargin(interval time.Duration) time.Duration {
	if interval < time.Second {
		return time.Second
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

func (m *Manager) gate(ctx context.Context, timeout time.Duration, invoke func(context.Context) error) error {
	if ctx == nil || invoke == nil || timeout <= 0 {
		return ErrActivationGate
	}
	gateCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := invoke(gateCtx); err != nil {
		return errors.Join(ErrActivationGate, err)
	}
	return nil
}

func (m *Manager) gateRequest(journal updateflow.Journal, candidate Release, worker hostdproto.Status) GateRequest {
	return GateRequest{TransactionID: journal.TransactionID, Previous: m.active, Candidate: candidate, Worker: worker}
}

func (m *Manager) record(ctx context.Context, phase EventPhase, journal updateflow.Journal, release Release, failure string) {
	if m.config.Events == nil {
		return
	}
	eventCtx := ctx
	if eventCtx == nil || eventCtx.Err() != nil {
		var cancel context.CancelFunc
		eventCtx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	_ = m.config.Events.RecordUpdateEvent(eventCtx, Event{At: m.now(), Phase: phase, TransactionID: journal.TransactionID, FromVersion: m.active.Version, ToVersion: release.Version, Failure: failure})
}

func safeFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "failed"
}

func (m *Manager) stage(ctx context.Context, release Release) (Release, error) {
	stream, err := m.config.Fetcher.Fetch(ctx, release)
	if err != nil {
		return Release{}, err
	}
	defer stream.Close()
	directory := filepath.Dir(m.config.BinaryStaged)
	//paperboat:allow-source-policy atomic-replacement owner=worker-update reason=same-directory-verified-runtime-download-staging
	pending, err := os.CreateTemp(directory, runtimeStagingPattern(release.Platform))
	if err != nil {
		return Release{}, err
	}
	pendingPath := pending.Name()
	defer os.Remove(pendingPath)
	// The downloaded Darwin package stays private while its canonical payload
	// is extracted. Native executable targets must be traversable by the
	// enrolled worker UID when hostd launches the candidate.
	stagingMode := os.FileMode(0o755)
	if release.Platform == "darwin" {
		stagingMode = 0o600
	}
	if err := pending.Chmod(stagingMode); err != nil {
		pending.Close()
		return Release{}, err
	}
	if err := pending.Chown(m.config.OwnerUID, m.config.OwnerGID); err != nil {
		pending.Close()
		return Release{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(pending, hash), io.LimitReader(stream, release.Length+1))
	if copyErr != nil || written != release.Length || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), release.SHA256) {
		pending.Close()
		return Release{}, ErrInvalidRelease
	}
	if err := pending.Sync(); err != nil {
		pending.Close()
		return Release{}, err
	}
	if err := pending.Close(); err != nil {
		return Release{}, err
	}
	if release.Platform == "darwin" {
		if m.config.ExtractPackage == nil {
			return Release{}, ErrInvalidConfig
		}
		if err := m.config.NativeVerifier.Verify(ctx, pendingPath, release.Platform, release.Architecture); err != nil {
			return Release{}, ErrInvalidRelease
		}
		extractionRoot, err := os.MkdirTemp(directory, ".paperboat-package-extraction-")
		if err != nil {
			return Release{}, err
		}
		defer os.RemoveAll(extractionRoot)
		if err := os.Chmod(extractionRoot, 0o700); err != nil {
			return Release{}, err
		}
		installedPath, err := m.config.ExtractPackage(ctx, pendingPath, extractionRoot)
		if err != nil {
			return Release{}, ErrInvalidRelease
		}
		if err := stageInstalledDarwinExecutable(installedPath, m.config.BinaryStaged, m.config.OwnerUID, m.config.OwnerGID); err != nil {
			return Release{}, ErrInvalidRelease
		}
		if err := binarytarget.Validate(m.config.BinaryStaged, release.Platform, release.Architecture); err != nil {
			return Release{}, ErrInvalidRelease
		}
		if err := m.config.NativeVerifier.Verify(ctx, m.config.BinaryStaged, release.Platform, release.Architecture); err != nil {
			return Release{}, ErrInvalidRelease
		}
		if err := syncDirectories(directory, filepath.Dir(m.config.BinaryStaged)); err != nil {
			return Release{}, err
		}
		return localReleaseIdentity(release, m.config.BinaryStaged)
	}
	if err := binarytarget.Validate(pendingPath, release.Platform, release.Architecture); err != nil {
		return Release{}, ErrInvalidRelease
	}
	if err := m.config.NativeVerifier.Verify(ctx, pendingPath, release.Platform, release.Architecture); err != nil {
		return Release{}, ErrInvalidRelease
	}
	//paperboat:allow-source-policy atomic-replacement owner=worker-update reason=verified-runtime-download-publication
	if err := os.Rename(pendingPath, m.config.BinaryStaged); err != nil {
		return Release{}, err
	}
	if err := syncDirectories(directory); err != nil {
		return Release{}, err
	}
	return release, nil
}

func localReleaseIdentity(release Release, path string) (Release, error) {
	file, err := os.Open(path)
	if err != nil {
		return Release{}, err
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(file, maxRuntimeBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written < 1 || written > maxRuntimeBytes {
		return Release{}, ErrInvalidRelease
	}
	release.SHA256 = hex.EncodeToString(hash.Sum(nil))
	release.Length = written
	return release, nil
}

func (m *Manager) authorizeRecovery(ctx context.Context, release Release, path string) error {
	if err := validateRelease(release); err != nil {
		return ErrInvalidRelease
	}
	if release.LocalSource == nil || release.LocalSource.Distribution == installsource.Official {
		if err := m.config.Fetcher.AuthorizeRecovery(ctx, release.Version, release.Platform, release.Architecture); err != nil {
			return err
		}
	}
	if err := safeRuntimeFile(path, m.config.OwnerUID, true); err != nil {
		return ErrUnsafeStorage
	}
	if !regularMatches(path, release.Length, release.SHA256) {
		return ErrInvalidRelease
	}
	if err := binarytarget.Validate(path, release.Platform, release.Architecture); err != nil {
		return ErrInvalidRelease
	}
	if err := m.config.NativeVerifier.Verify(ctx, path, release.Platform, release.Architecture); err != nil {
		return ErrInvalidRelease
	}
	return nil
}

func runtimeStagingPattern(platform string) string {
	if platform == "darwin" {
		return ".paperboat-runtime-*.pkg"
	}
	return ".paperboat-runtime-*"
}

func stageInstalledDarwinExecutable(source, destination string, ownerUID, ownerGID int) error {
	if source == "" || !filepath.IsAbs(source) || filepath.Clean(source) != source || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return ErrInvalidRelease
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxRuntimeBytes {
		return ErrInvalidRelease
	}
	directory := filepath.Dir(destination)
	//paperboat:allow-source-policy atomic-replacement owner=worker-update reason=same-directory-verified-darwin-executable-staging
	pending, err := os.CreateTemp(directory, ".paperboat-darwin-executable-")
	if err != nil {
		return err
	}
	pendingPath := pending.Name()
	defer os.Remove(pendingPath)
	if err := pending.Chmod(0o755); err != nil {
		_ = pending.Close()
		return err
	}
	if err := pending.Chown(ownerUID, ownerGID); err != nil {
		_ = pending.Close()
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		_ = pending.Close()
		return err
	}
	written, copyErr := io.Copy(pending, io.LimitReader(input, maxRuntimeBytes+1))
	closeErr := input.Close()
	if copyErr != nil || closeErr != nil || written != info.Size() {
		_ = pending.Close()
		return ErrInvalidRelease
	}
	if err := pending.Sync(); err != nil {
		_ = pending.Close()
		return err
	}
	if err := pending.Close(); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=worker-update reason=verified-darwin-package-executable-staging
	if err := os.Rename(pendingPath, destination); err != nil {
		return err
	}
	return nil
}

func (m *Manager) removeStaged() error {
	return removeRuntimeFile(m.config.BinaryStaged, m.config.OwnerUID)
}

func (m *Manager) newJournal() updateflow.Journal {
	return withActiveRelease(updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: transactionID(), Stage: updateflow.StageIdle,
		ActiveVersion: m.active.Version, BootID: "hostd", StageUpdatedAt: m.now()}, m.active)
}

func (m *Manager) idleJournal(from updateflow.Journal, failure updateflow.Failure, quarantine string) updateflow.Journal {
	journal := withActiveRelease(updateflow.Journal{Schema: updateflow.SchemaV1, TransactionID: from.TransactionID, Stage: updateflow.StageIdle,
		ActiveVersion: m.active.Version, BootID: from.BootID, StageUpdatedAt: m.now(), RollbackCount: from.RollbackCount, LastFailure: failure}, m.active)
	if quarantine != "" {
		journal = withRelease(journal, Release{Version: quarantine, SHA256: from.CandidateDigest, Length: from.CandidateLength, Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: from.HostdAPIMin, HostdAPIMax: from.HostdAPIMax, RuntimeAPIMin: from.RuntimeAPIMin, RuntimeAPIMax: from.RuntimeAPIMax}, m.config.BinaryStaged)
	}
	return journal
}

func (m *Manager) transition(journal updateflow.Journal, stage updateflow.Stage) (updateflow.Journal, error) {
	next, err := journal.Transition(stage, m.now())
	if err == nil && stage == updateflow.StageMonitoring {
		next.HealthDeadline = m.now().Add(m.config.MonitorWindow)
	}
	return next, err
}
func (m *Manager) now() time.Time { return m.config.Now().UTC() }
func (m *Manager) write(journal updateflow.Journal) error {
	return m.config.WriteJournal(m.config.StatePath, journal, m.config.OwnerUID, m.config.OwnerGID)
}

func (m *Manager) quarantinedLocked() (string, error) {
	journal, err := updateflow.Load(m.config.StatePath)
	if journalMissingOnly(err) {
		return "", nil
	}
	if err != nil {
		return "", errors.Join(ErrBlocked, err)
	}
	if journal.Stage == updateflow.StageIdle && journal.LastFailure != updateflow.FailureNone && journal.CandidateVersion != "" {
		return journal.CandidateVersion, nil
	}
	return "", nil
}

func withRelease(journal updateflow.Journal, release Release, path string) updateflow.Journal {
	journal.SupervisorMaintenance, journal.OwnerMaintenanceGraceSeconds = release.SupervisorMaintenance, release.OwnerMaintenanceGraceSeconds
	journal.CandidateVersion, journal.CandidateDigest, journal.CandidateLength, journal.StagedPath = release.Version, release.SHA256, release.Length, path
	journal.CandidateManifestDigest = release.ManifestSHA256
	journal.CandidatePolicy = nil
	if release.ManifestSHA256 != "" {
		policy := activationPolicy(release)
		journal.CandidatePolicy = &policy
	}
	journal.HostdAPIMin, journal.HostdAPIMax, journal.RuntimeAPIMin, journal.RuntimeAPIMax = release.HostdAPIMin, release.HostdAPIMax, release.RuntimeAPIMin, release.RuntimeAPIMax
	return journal
}

func withActiveRelease(journal updateflow.Journal, release Release) updateflow.Journal {
	journal.ActiveVersion = release.Version
	journal.ActiveSource = release.LocalSource
	journal.ActiveDigest, journal.ActiveLength = release.SHA256, release.Length
	journal.ActiveHostdAPIMin, journal.ActiveHostdAPIMax = release.HostdAPIMin, release.HostdAPIMax
	journal.ActiveRuntimeAPIMin, journal.ActiveRuntimeAPIMax = release.RuntimeAPIMin, release.RuntimeAPIMax
	return journal
}

// ActiveReleaseFromJournal returns release identity that was previously
// derived from signed metadata and durably bound to the update transaction.
func ActiveReleaseFromJournal(path, version string) (Release, error) {
	journal, err := updateflow.Load(path)
	if err != nil {
		return Release{}, err
	}
	if journal.ActiveVersion != version || journal.ActiveDigest == "" {
		return Release{}, ErrInvalidRelease
	}
	return completeJournalRelease(activeReleaseFromJournal(journal))
}

func activeReleaseFromJournal(journal updateflow.Journal) Release {
	return Release{
		Version: journal.ActiveVersion, SHA256: journal.ActiveDigest, Length: journal.ActiveLength, LocalSource: journal.ActiveSource,
		Platform: runtime.GOOS, Architecture: runtime.GOARCH,
		HostdAPIMin: journal.ActiveHostdAPIMin, HostdAPIMax: journal.ActiveHostdAPIMax,
		RuntimeAPIMin: journal.ActiveRuntimeAPIMin, RuntimeAPIMax: journal.ActiveRuntimeAPIMax,
	}
}

func completeJournalRelease(release Release) (Release, error) {
	component := ComponentTarget{SHA256: release.SHA256, Length: release.Length, Platform: release.Platform, Architecture: release.Architecture}
	release.CLISHA256, release.CLILength, release.CLIPlatform, release.CLIArchitecture = release.SHA256, release.Length, release.Platform, release.Architecture
	release.Hostd, release.Updater, release.Launcher = component, component, component
	if err := validateRelease(release); err != nil {
		return Release{}, err
	}
	return release, nil
}

func releaseFromJournal(j updateflow.Journal) Release {
	release := Release{Version: j.CandidateVersion, SHA256: j.CandidateDigest, ManifestSHA256: j.CandidateManifestDigest, Length: j.CandidateLength, Platform: runtime.GOOS, Architecture: runtime.GOARCH, HostdAPIMin: j.HostdAPIMin, HostdAPIMax: j.HostdAPIMax, RuntimeAPIMin: j.RuntimeAPIMin, RuntimeAPIMax: j.RuntimeAPIMax}
	release.SupervisorMaintenance, release.OwnerMaintenanceGraceSeconds = j.SupervisorMaintenance, j.OwnerMaintenanceGraceSeconds
	if p := j.CandidatePolicy; p != nil {
		release.CanaryPath, release.CanaryStatus, release.CanarySamples = p.CanaryPath, p.CanaryStatus, p.CanarySamples
		release.CanaryTimeout, release.DrainTimeout, release.StabilityWindow, release.StabilityInterval, release.RollbackTimeout = p.CanaryTimeout, p.DrainTimeout, p.StabilityWindow, p.StabilityInterval, p.RollbackTimeout
	}
	return release
}
func workerID(version string) string { return "runtime-" + version }
func matches(status hostdproto.Status, state hostdproto.State, id string, epoch uint64) bool {
	return status.State == state && status.WorkerID == id && (epoch == 0 || status.Epoch == epoch)
}

func validateConfig(config Config) error {
	if config.Fetcher == nil || config.ApplyPrepared == nil || config.Gate == nil || config.Hostd == nil || config.Health == nil || config.OwnerUID < 0 || config.OwnerGID < 0 || config.MonitorWindow <= 0 || config.HealthInterval <= 0 || config.HealthInterval > config.MonitorWindow || config.HealthInterval > 30*time.Second || config.CanaryTimeout <= 0 || config.CanaryTimeout > 30*time.Second || config.DrainTimeout <= 0 || config.DrainTimeout > 2*time.Minute || config.RollbackTimeout <= 0 || config.RollbackTimeout > 2*time.Minute || validateRelease(config.Active) != nil {
		return ErrInvalidConfig
	}
	paths := []string{config.StatePath, config.Binary, config.BinaryStaged}
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || filepath.Base(path) == string(filepath.Separator) {
			return ErrInvalidConfig
		}
		if err := secureDirectory(filepath.Dir(path), config.OwnerUID); err != nil {
			return err
		}
	}
	if config.Binary == config.BinaryStaged {
		return ErrInvalidConfig
	}
	if err := safeRuntimeFile(config.Binary, config.OwnerUID, true); err != nil {
		return err
	}
	if binarytarget.Validate(config.Binary, config.Active.Platform, config.Active.Architecture) != nil {
		return ErrUnsafeStorage
	}
	if !regularMatches(config.Binary, config.Active.Length, config.Active.SHA256) && !matchesInterruptedCandidate(config) {
		return ErrUnsafeStorage
	}
	if config.Active.Platform == "darwin" && config.ExtractPackage == nil {
		return ErrUnsafeStorage
	}
	return nil
}

// A restarted updater still owns the previous logical release until recovery
// resolves hostd's fence. Permit only the exact candidate durably bound to that
// previous release in a protected post-cutover journal.
func samePreparedBaseline(journal updateflow.Journal, active Release) bool {
	if journal.ActiveVersion != active.Version || journal.ActiveDigest != active.SHA256 || journal.ActiveLength != active.Length || (journal.ActiveSource == nil) != (active.LocalSource == nil) {
		return false
	}
	return journal.ActiveSource == nil || *journal.ActiveSource == *active.LocalSource
}

func matchesInterruptedCandidate(config Config) bool {
	info, err := os.Lstat(config.StatePath)
	if err != nil || !info.Mode().IsRegular() || fileOwner(info) != config.OwnerUID || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return false
	}
	journal, err := updateflow.Load(config.StatePath)
	preparedNative := config.ApplyPrepared != nil && journal.Stage == updateflow.StageAwaitingApproval && journal.CandidateID != "" && journal.ApprovedCandidateID == journal.CandidateID
	if err != nil || !preparedNative {
		return false
	}
	if !samePreparedBaseline(journal, config.Active) {
		return false
	}
	candidate := releaseFromJournal(journal)
	return validateRelease(candidate) == nil && regularMatches(config.Binary, candidate.Length, candidate.SHA256)
}

func validateRelease(release Release) error {
	if !(validVersion(release.Version) || validLocalRelease(release)) || release.LocalSource != nil && !validLocalRelease(release) || len(release.SHA256) != 64 || release.Length < 1 || release.Length > maxRuntimeBytes || release.Platform != runtime.GOOS || release.Architecture != runtime.GOARCH || !hexDigest(release.SHA256) || invalidRequiredAPIRange(release.HostdAPIMin, release.HostdAPIMax) || invalidRequiredAPIRange(release.RuntimeAPIMin, release.RuntimeAPIMax) {
		return ErrInvalidRelease
	}
	return nil
}

func invalidAPIRange(minimum, maximum uint16) bool { return minimum > maximum || maximum > 1024 }
func invalidRequiredAPIRange(minimum, maximum uint16) bool {
	return minimum == 0 || invalidAPIRange(minimum, maximum)
}
func hexDigest(value string) bool { _, err := hex.DecodeString(value); return err == nil }
func validVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}
func compareVersion(left, right string) int {
	l, r := strings.Split(left, "."), strings.Split(right, ".")
	for i := range 4 {
		lv, _ := strconv.ParseUint(l[i], 10, 32)
		rv, _ := strconv.ParseUint(r[i], 10, 32)
		if lv < rv {
			return -1
		}
		if lv > rv {
			return 1
		}
	}
	return 0
}
func transactionID() string { return fmt.Sprintf("txn-%d", time.Now().UnixNano()) }

func secureDirectory(path string, owner int) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || fileOwner(info) != owner {
		return ErrUnsafeStorage
	}
	return nil
}
func safeRuntimeFile(path string, owner int, required bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || fileOwner(info) != owner {
		return ErrUnsafeStorage
	}
	return nil
}
func removeRuntimeFile(path string, owner int) error {
	if err := safeRuntimeFile(path, owner, false); err != nil {
		return err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectories(filepath.Dir(path))
}
func regularMatches(path string, length int64, digest string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != length {
		return false
	}
	sum := sha256.New()
	if _, err = io.Copy(sum, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(sum.Sum(nil)), digest)
}
func syncDirectories(paths ...string) error {
	seen := map[string]struct{}{}
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		directory, err := os.Open(path)
		if err != nil {
			return err
		}
		err = directory.Sync()
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

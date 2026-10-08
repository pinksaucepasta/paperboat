package bootstrap

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

const resumeSchema = "paperboat.machine-resume/v1"

var (
	ErrResumeNotFound      = errors.New("machine bootstrap resume state was not found")
	ErrResumeBinding       = errors.New("machine bootstrap resume state does not match this machine or enrollment")
	ErrResumeTokenChanged  = fmt.Errorf("%w: enrollment token changed", ErrResumeBinding)
	ErrResumeExpired       = errors.New("machine bootstrap resume state has expired")
	ErrResumeTokenRequired = errors.New("machine bootstrap resume state requires the original enrollment token")
)

type resumeStorageOperation uint8

const (
	resumeStoragePrepare resumeStorageOperation = iota + 1
	resumeStorageInspect
	resumeStorageRead
	resumeStorageWrite
	resumeStorageRemove
)

type resumeStorageFailure struct {
	operation resumeStorageOperation
	cause     error
}

func (failure resumeStorageFailure) Error() string {
	switch failure.operation {
	case resumeStoragePrepare:
		return "machine bootstrap resume state could not be prepared"
	case resumeStorageInspect:
		return "machine bootstrap resume state could not be inspected"
	case resumeStorageRead:
		return "machine bootstrap resume state could not be read"
	case resumeStorageWrite:
		return "machine bootstrap resume state could not be saved"
	case resumeStorageRemove:
		return "machine bootstrap resume state could not be cleared"
	default:
		return "machine bootstrap resume state operation failed"
	}
}

func (failure resumeStorageFailure) Unwrap() error   { return failure.cause }
func (resumeStorageFailure) DiagnosticStage() string { return "reconciliation" }
func (resumeStorageFailure) DiagnosticCode() string  { return "command_failed" }

type resumeBindingFailure struct{ cause error }

func (failure resumeBindingFailure) Error() string { return ErrResumeBinding.Error() }
func (failure resumeBindingFailure) Unwrap() error { return failure.cause }
func (resumeBindingFailure) Is(target error) bool  { return target == ErrResumeBinding }

func resumeBindingError(cause error) error {
	if cause == nil {
		return ErrResumeBinding
	}
	return resumeBindingFailure{cause: cause}
}

func resumeStorageError(operation resumeStorageOperation, cause error) error {
	if cause == nil {
		return nil
	}
	return resumeStorageFailure{operation: operation, cause: cause}
}

// ResumeRecord is the protected, local journal for a one-shot machine
// enrollment. It never stores the enrollment token itself, only its digest.
// Material is retained until the host installation commits so ordinary retries
// remain local; the verifier-bound server recovery path is reserved for a lost
// or expired material response.
type ResumeRecord struct {
	Schema                  string    `json:"schema"`
	ServerURL               string    `json:"server_url"`
	PublicIdentityKey       string    `json:"public_identity_key"`
	EnrollmentTokenSHA      string    `json:"enrollment_token_sha256"`
	EnrollmentTokenRequired bool      `json:"enrollment_token_required,omitempty"`
	Alias                   string    `json:"alias"`
	Verifier                string    `json:"verifier"`
	PairingExpiresAt        time.Time `json:"pairing_expires_at"`
	PairingStarted          bool      `json:"pairing_started,omitempty"`
	Material                *Material `json:"material,omitempty"`
	ClientInstalled         bool      `json:"client_installed,omitempty"`
	RuntimeEnrolled         bool      `json:"runtime_enrolled,omitempty"`
	// RuntimeReady checkpoints local finalization before privileged commit.
	// Recovery uses the existing runtime identity, never expired enrollment authority.
	RuntimeReady bool `json:"runtime_ready,omitempty"`
	// RuntimeListenAddress is selected locally and survives renewed server material.
	RuntimeListenAddress  string `json:"runtime_listen_address,omitempty"`
	AuthenticatedSetup    bool   `json:"authenticated_setup,omitempty"`
	SetupOperationID      string `json:"setup_operation_id,omitempty"`
	ExpectedUserMachineID string `json:"expected_user_machine_id,omitempty"`
	ExpectedGeneration    int64  `json:"expected_installation_generation,omitempty"`
	// RequestedArtifact binds an authenticated setup operation to the signed
	// release target that the server was asked to issue. Material.Artifact is
	// retained as the authoritative binding after issuance; this field closes
	// the pre-material window where reusing an operation for a different target
	// would otherwise create an idempotency conflict or cross-release replay.
	RequestedArtifact *ArtifactTarget `json:"requested_artifact,omitempty"`
}

func ResumePath(stateRoot string) string {
	return filepath.Join(stateRoot, "bootstrap-resume.json")
}

// NewResumeRecord creates the pre-pairing journal. Keeping the verifier lets
// a process that dies after server pairing but before material delivery resume
// polling without attempting a second pairing.
func NewResumeRecord(serverURL, publicIdentityKey, enrollmentToken, alias, verifier string, expiresAt time.Time) ResumeRecord {
	return ResumeRecord{
		Schema:                  resumeSchema,
		ServerURL:               strings.TrimRight(strings.TrimSpace(serverURL), "/"),
		PublicIdentityKey:       strings.TrimSpace(publicIdentityKey),
		EnrollmentTokenSHA:      enrollmentTokenDigest(enrollmentToken),
		EnrollmentTokenRequired: strings.TrimSpace(enrollmentToken) != "",
		Alias:                   strings.TrimSpace(alias),
		Verifier:                strings.TrimSpace(verifier),
		PairingExpiresAt:        expiresAt,
	}
}

func SaveResume(stateRoot string, record ResumeRecord) error {
	if err := validateResumeRecord(record, false); err != nil {
		return err
	}
	if err := ensureResumeDirectory(stateRoot); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(ResumePath(stateRoot), encoded, atomicfile.CurrentOwnerOptions(0o600)); err != nil {
		return resumeStorageError(resumeStorageWrite, err)
	}
	return nil
}

// LoadResume validates the file and its immutable binding. An empty token is
// accepted only after the server-pairing phase is durably recorded, which is
// necessary when a token-file installer consumed the local file before a
// later process failed. Before pairing, a token-backed journal must still be
// given its original token so it cannot silently become an identity pairing.
func LoadResume(stateRoot, serverURL, publicIdentityKey, enrollmentToken, alias string, now time.Time) (ResumeRecord, error) {
	if !filepath.IsAbs(stateRoot) || now.IsZero() {
		return ResumeRecord{}, ErrResumeBinding
	}
	record, err := loadResumeDocument(stateRoot)
	if err != nil {
		return ResumeRecord{}, err
	}
	if strings.TrimRight(strings.TrimSpace(serverURL), "/") != record.ServerURL ||
		strings.TrimSpace(publicIdentityKey) != record.PublicIdentityKey ||
		strings.TrimSpace(alias) != record.Alias {
		return ResumeRecord{}, ErrResumeBinding
	}
	if record.requiresEnrollmentToken() && strings.TrimSpace(enrollmentToken) == "" && !record.PairingStarted {
		return ResumeRecord{}, ErrResumeTokenRequired
	}
	if strings.TrimSpace(enrollmentToken) != "" && enrollmentTokenDigest(enrollmentToken) != record.EnrollmentTokenSHA {
		return record, ErrResumeTokenChanged
	}
	if record.Material != nil && !now.UTC().Before(record.Material.ExpiresAt) {
		return record, ErrResumeExpired
	}
	if record.Material == nil && !record.PairingExpiresAt.IsZero() && !now.UTC().Before(record.PairingExpiresAt) {
		return record, ErrResumeExpired
	}
	return record, nil
}

// ResumeMatchesEnrollmentToken classifies a fresh-installer retry without
// weakening the journal's normal machine and enrollment binding checks. A
// missing journal or a different valid token requests a new reset; malformed
// protected state fails closed so recoverable material is never deleted.
func ResumeMatchesEnrollmentToken(stateRoot, enrollmentToken string) (bool, error) {
	if !filepath.IsAbs(stateRoot) || strings.TrimSpace(enrollmentToken) == "" {
		return false, ErrResumeBinding
	}
	record, err := loadResumeDocument(stateRoot)
	if errors.Is(err, ErrResumeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enrollmentTokenDigest(enrollmentToken) == record.EnrollmentTokenSHA, nil
}

// PrepareAuthenticatedSetupResume creates or reuses the protected verifier
// journal for an authenticated Host setup. It replaces an older journal only
// when every immutable machine binding matches and the journal is expired with
// no material or locally committed installation progress.
func PrepareAuthenticatedSetupResume(stateRoot, serverURL, publicIdentityKey, alias, machineID string, installationGeneration int64, artifact ArtifactTarget, now time.Time) (ResumeRecord, error) {
	serverURL = strings.TrimRight(strings.TrimSpace(serverURL), "/")
	publicIdentityKey, alias, machineID = strings.TrimSpace(publicIdentityKey), strings.TrimSpace(alias), strings.TrimSpace(machineID)
	if !filepath.IsAbs(stateRoot) || !validResumeServer(serverURL) || publicIdentityKey == "" || alias == "" || machineID == "" || installationGeneration < 1 || now.IsZero() {
		return ResumeRecord{}, ErrResumeBinding
	}
	if err := VerifyArtifactTarget(artifact); err != nil {
		return ResumeRecord{}, resumeBindingError(err)
	}
	existing, err := loadResumeDocument(stateRoot)
	if err == nil {
		exactBase := existing.ServerURL == serverURL && existing.PublicIdentityKey == publicIdentityKey && existing.Alias == alias
		exactAuthenticatedBinding := exactBase && existing.ExpectedUserMachineID == machineID && existing.ExpectedGeneration == installationGeneration
		expired := !now.UTC().Before(existing.PairingExpiresAt)
		if existing.Material != nil && !now.UTC().Before(existing.Material.ExpiresAt) {
			expired = true
		}
		if existing.AuthenticatedSetup {
			if !exactAuthenticatedBinding {
				return ResumeRecord{}, ErrResumeBinding
			}
			progress := existing.RuntimeEnrolled || existing.ClientInstalled
			if !authenticatedSetupArtifactMatches(existing, artifact) {
				// A target change must never replay the old verifier and
				// idempotency operation. Only an expired journal with no local
				// installation progress may be replaced with a new operation.
				if progress || !expired {
					return ResumeRecord{}, ErrResumeBinding
				}
			} else {
				if !expired {
					return existing, nil
				}
				// A server-issued material response is durable recovery
				// authority. Reuse the exact operation only when the signed
				// artifact is unchanged and no local installation checkpoint
				// has been committed.
				if existing.Material != nil && !progress {
					return existing, nil
				}
				if progress {
					return ResumeRecord{}, ErrResumeBinding
				}
			}
		} else if !exactBase || !existing.PairingStarted || !expired || existing.Material != nil || existing.RuntimeEnrolled || existing.ClientInstalled {
			return ResumeRecord{}, ErrResumeBinding
		}
		if clearErr := ClearResume(stateRoot); clearErr != nil {
			return ResumeRecord{}, clearErr
		}
	} else if !errors.Is(err, ErrResumeNotFound) {
		return ResumeRecord{}, err
	}
	verifierBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, verifierBytes); err != nil {
		return ResumeRecord{}, err
	}
	operation, err := uuid.NewRandom()
	if err != nil {
		return ResumeRecord{}, err
	}
	record := NewResumeRecord(serverURL, publicIdentityKey, "", alias, base64.RawURLEncoding.EncodeToString(verifierBytes), now.UTC().Add(15*time.Minute))
	record.AuthenticatedSetup = true
	record.SetupOperationID = "operation_" + operation.String()
	record.ExpectedUserMachineID = machineID
	record.ExpectedGeneration = installationGeneration
	record.RequestedArtifact = cloneArtifactTarget(artifact)
	if err := SaveResume(stateRoot, record); err != nil {
		return ResumeRecord{}, err
	}
	return record, nil
}

func authenticatedSetupArtifactMatches(record ResumeRecord, requested ArtifactTarget) bool {
	bound := record.RequestedArtifact
	if bound == nil && record.Material != nil {
		bound = record.Material.Artifact
	}
	return bound != nil && *bound == requested
}

func cloneArtifactTarget(target ArtifactTarget) *ArtifactTarget {
	copy := target
	return &copy
}

func loadResumeDocument(stateRoot string) (ResumeRecord, error) {
	path := ResumePath(stateRoot)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ResumeRecord{}, ErrResumeNotFound
	}
	if err != nil {
		return ResumeRecord{}, resumeStorageError(resumeStorageInspect, err)
	}
	if !secureResumeFile(path, info, 512<<10) {
		return ResumeRecord{}, ErrResumeBinding
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return ResumeRecord{}, resumeStorageError(resumeStorageRead, err)
	}
	var record ResumeRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return ResumeRecord{}, resumeBindingError(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ResumeRecord{}, ErrResumeBinding
		}
		return ResumeRecord{}, resumeBindingError(err)
	}
	if err := validateResumeRecord(record, true); err != nil {
		return ResumeRecord{}, resumeBindingError(err)
	}
	return record, nil
}

func ClearResume(stateRoot string) error {
	path := ResumePath(stateRoot)
	if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return resumeStorageError(resumeStorageInspect, err)
	} else if !secureResumeFile(path, info, 512<<10) {
		return ErrResumeBinding
	}
	if err := os.Remove(path); err != nil {
		return resumeStorageError(resumeStorageRemove, err)
	}
	return nil
}

// ValidateRecoveredMaterial binds renewed/replayed credentials to the exact
// machine installation already stored in the protected journal. Credentials
// and release artifacts may rotate, but machine ownership and installation
// generation must not change under the same verifier.
func ValidateRecoveredMaterial(previous, recovered Material, runtimeEnrolled bool) error {
	if previous.UserMachineID != recovered.UserMachineID ||
		previous.PairingID != recovered.PairingID ||
		previous.EnvironmentID != recovered.EnvironmentID ||
		runtimeEnrolled && previous.HelperID != recovered.HelperID ||
		previous.InstallationGeneration != recovered.InstallationGeneration ||
		normalizeBootstrapURL(previous.ControlURL) != normalizeBootstrapURL(recovered.ControlURL) {
		return fmt.Errorf("%w: recovered material changed the bound machine installation", ErrResumeBinding)
	}
	return nil
}

// ValidateAuthenticatedSetupMaterial binds authenticated machine material to the
// exact setup transition and, on recovery, to the artifact that was verified
// before the one-shot installation authority was issued.
func ValidateAuthenticatedSetupMaterial(record ResumeRecord, material Material) error {
	if !record.AuthenticatedSetup || material.UserMachineID != record.ExpectedUserMachineID || material.InstallationGeneration != record.ExpectedGeneration {
		return ErrResumeBinding
	}
	if record.Material == nil {
		return nil
	}
	if err := ValidateRecoveredMaterial(*record.Material, material, record.RuntimeEnrolled); err != nil {
		return err
	}
	if record.Material.Artifact == nil || material.Artifact == nil || *record.Material.Artifact != *material.Artifact {
		return fmt.Errorf("%w: authenticated Host recovery changed the verified artifact", ErrResumeBinding)
	}
	return nil
}

func enrollmentTokenDigest(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(digest[:])
}

func (record ResumeRecord) requiresEnrollmentToken() bool {
	// The digest check keeps journals written before the explicit boolean field
	// backwards compatible while still distinguishing an identity pairing from
	// a token-backed pairing before the server has accepted CreatePairing.
	return record.EnrollmentTokenRequired || record.EnrollmentTokenSHA != enrollmentTokenDigest("")
}

// RequiresEnrollmentTokenForRetry reports whether a retry would otherwise
// downgrade a token-backed pairing to an unauthenticated identity pairing.
// Once PairingStarted is true, an empty token is allowed because the server
// has already accepted the one-shot credential and the verifier is the resume
// binding. Callers must still refuse a new CreatePairing without the token.
func (record ResumeRecord) RequiresEnrollmentTokenForRetry(token string) bool {
	return record.requiresEnrollmentToken() && strings.TrimSpace(token) == ""
}

func validateResumeRecord(record ResumeRecord, loaded bool) error {
	if record.RuntimeReady && (!record.RuntimeEnrolled || !record.ClientInstalled || record.Material == nil || record.RuntimeListenAddress == "") {
		return ErrResumeBinding
	}
	if record.RuntimeListenAddress != "" {
		address, err := netip.ParseAddrPort(record.RuntimeListenAddress)
		if err != nil || !address.Addr().IsLoopback() || address.Port() == 0 || record.Material == nil {
			return ErrResumeBinding
		}
	}
	if record.Schema != resumeSchema || !validResumeServer(record.ServerURL) || record.PublicIdentityKey == "" || len(record.EnrollmentTokenSHA) != sha256.Size*2 || record.Alias == "" || len(record.Verifier) < 32 || record.PairingExpiresAt.IsZero() {
		return ErrResumeBinding
	}
	if _, err := hex.DecodeString(record.EnrollmentTokenSHA); err != nil {
		return ErrResumeBinding
	}
	if record.AuthenticatedSetup {
		if record.requiresEnrollmentToken() || len(record.SetupOperationID) < 8 || len(record.SetupOperationID) > 128 || record.ExpectedUserMachineID == "" || record.ExpectedGeneration < 1 {
			return ErrResumeBinding
		}
		if record.RequestedArtifact != nil {
			if err := VerifyArtifactTarget(*record.RequestedArtifact); err != nil {
				return resumeBindingError(err)
			}
		}
	} else if record.SetupOperationID != "" || record.ExpectedUserMachineID != "" || record.ExpectedGeneration != 0 {
		return ErrResumeBinding
	}
	if loaded && record.Material == nil && record.ClientInstalled || record.RuntimeEnrolled && record.Material == nil {
		return ErrResumeBinding
	}
	if record.Material != nil {
		if !record.PairingStarted {
			return fmt.Errorf("%w: material exists before pairing started", ErrResumeBinding)
		}
		if err := validateMaterialFreshness(*record.Material, !loaded && !record.RuntimeReady); err != nil {
			return resumeBindingError(err)
		}

		if strings.TrimRight(strings.TrimSpace(record.Material.ControlURL), "/") != record.ServerURL {
			return fmt.Errorf("%w: material control URL does not match journal server URL", ErrResumeBinding)
		}
	}
	return nil
}

func validResumeServer(value string) bool {
	return strings.HasPrefix(value, "https://") && !strings.ContainsAny(value, "\x00\r\n")
}

func ensureResumeDirectory(stateRoot string) error {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return ErrResumeBinding
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return resumeStorageError(resumeStoragePrepare, err)
	}
	info, err := os.Lstat(stateRoot)
	if err != nil {
		return resumeStorageError(resumeStorageInspect, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrResumeBinding
	}
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		return resumeStorageError(resumeStoragePrepare, err)
	}
	return nil
}

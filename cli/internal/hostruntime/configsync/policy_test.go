package configsync

import (
	"errors"
	"testing"
	"time"
)

func TestValidateRuntimeDescriptorAcceptsEmptyManifestContract(t *testing.T) {
	credential := Credential{
		AssignmentID: "assignment", AssignmentVersion: 1, EnvironmentID: "environment", MachineID: "helper",
		WarningRevision: "warning",
	}
	descriptor := RuntimeDescriptor{
		WriteMode: "read_only", Mode: ModePullOnly, RepositoryID: "repository", AssignmentID: credential.AssignmentID, AssignmentVersion: credential.AssignmentVersion,
		EnvironmentID: credential.EnvironmentID, MachineID: credential.MachineID, InstallationGeneration: 1,
		WarningRevision: credential.WarningRevision,
		Policy: RuntimePolicy{
			Format: "paperboat-config-plaintext-v1", Revision: "policy",
			ManifestContract: ManifestContractVersion, ManifestMaxBytes: DefaultManifestMaxBytes,
			ManifestMaxLines: DefaultManifestMaxLines, ManifestMaxPatternBytes: DefaultManifestMaxPatternBytes,
			MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Debounce: time.Second,
			MinimumPushInterval: time.Minute, MaximumDirtyDelay: time.Minute,
			RemotePollInterval: time.Minute, RetryLimit: 1,
			ShutdownFlushTimeout: time.Second, SummaryLimit: 10,
		},
	}

	if err := validateRuntimeDescriptor(descriptor, credential); err != nil {
		t.Fatalf("manifest descriptor rejected: %v", err)
	}
	// The server publishes a watched write within seconds: a push floor of a few seconds is valid, one below the debounce is not.
	descriptor.Policy.MinimumPushInterval = 2 * time.Second
	if err := validateRuntimeDescriptor(descriptor, credential); err != nil {
		t.Fatalf("seconds push floor rejected: %v", err)
	}
	descriptor.Policy.MinimumPushInterval = descriptor.Policy.Debounce - 1
	if err := validateRuntimeDescriptor(descriptor, credential); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("push floor below debounce error = %v", err)
	}
	descriptor.Policy.MinimumPushInterval = time.Minute
	descriptor.Policy.ManifestMaxLines = 0
	if err := validateRuntimeDescriptor(descriptor, credential); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("invalid manifest limit error = %v", err)
	}
}

func TestMandatoryExcludedHonorsExactRuntimeRoots(t *testing.T) {
	policy := RuntimePolicy{RuntimeExclusionRoots: []string{"Library/Application Support/paperboat/helper[state]"}}
	if !mandatoryExcluded("Library/Application Support/paperboat/helper[state]/identity.json", policy) {
		t.Fatal("runtime identity path was not excluded")
	}
	if mandatoryExcluded("Library/Application Support/paperboat/helper-state/identity.json", policy) {
		t.Fatal("runtime exclusion matched a sibling prefix")
	}
}

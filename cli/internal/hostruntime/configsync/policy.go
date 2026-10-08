package configsync

import (
	"errors"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

var ErrPolicyInvalid = errors.New("invalid config sync policy")

var requiredMandatoryExclusions = []string{
	".paperboat-*", "**/.paperboat-*", ".git", "**/.git", ".paperboat", "**/.paperboat",
	".config/paperboat", ".config/paperboat/**",
	".local/bin/pb",
	".config/systemd/user/paperboat-runtime-host.service",
	".config/systemd/user/default.target.wants/paperboat-runtime-host.service",
	"Library/LaunchAgents/com.pinksaucepasta.paperboat.runtime-host.plist",
	".ssh", "**/.ssh", ".gnupg", "**/.gnupg",
	".aws", "**/.aws", ".kube", "**/.kube", ".docker/config.json",
	".git-credentials", "**/.git-credentials", ".netrc", "**/.netrc",
	".env", ".env.*", "**/.env", "**/.env.*", "**/credentials", "**/credentials.*",
	"**/*.db", "**/*.sqlite", "**/*.log", "**/*.tmp", "**/*_history",
}

func validateRuntimeDescriptor(descriptor RuntimeDescriptor, credential Credential) error {
	policy := descriptor.Policy
	if (descriptor.WriteMode != "read_only" && descriptor.WriteMode != "leased_writes") || !descriptor.Mode.Valid() ||
		descriptor.AssignmentVersion < 1 || descriptor.AssignmentVersion != credential.AssignmentVersion || descriptor.RepositoryID == "" || descriptor.AssignmentID != credential.AssignmentID ||
		((descriptor.PullRepositoryID != "" || descriptor.PushRepositoryID != "") &&
			((descriptor.Mode != ModePushOnly && descriptor.PullRepositoryID == "") ||
				(descriptor.Mode != ModePullOnly && descriptor.PushRepositoryID == ""))) ||
		descriptor.EnvironmentID != credential.EnvironmentID || descriptor.MachineID != credential.MachineID ||
		descriptor.InstallationGeneration < 1 || descriptor.SyncRevisionFloor < 0 ||
		descriptor.WarningRevision != credential.WarningRevision ||
		policy.Format != "paperboat-config-plaintext-v1" || policy.Revision == "" ||
		policy.MaxFileBytes < 1 || policy.MaxFileBytes > 100<<20 ||
		policy.MaxBatchBytes < policy.MaxFileBytes || policy.MaxBatchBytes > 500<<20 ||
		policy.Debounce < time.Second || policy.Debounce > 5*time.Minute ||
		policy.MinimumPushInterval < time.Minute || policy.MinimumPushInterval > 24*time.Hour ||
		policy.MaximumDirtyDelay < policy.Debounce || policy.MaximumDirtyDelay > 24*time.Hour ||
		policy.RemotePollInterval < time.Second || policy.RemotePollInterval > time.Hour ||
		policy.RetryLimit < 1 || policy.RetryLimit > 20 ||
		policy.ShutdownFlushTimeout < time.Second || policy.ShutdownFlushTimeout > 10*time.Minute ||
		policy.SummaryLimit < 1 || policy.SummaryLimit > 1000 ||
		policy.ManifestContract != ManifestContractVersion ||
		!validManifestLimits(policy.ManifestLimits()) {
		return ErrPolicyInvalid
	}
	for _, root := range policy.AbsoluteRuntimeExclusionRoots {
		if !canonicalAbsolutePath(root) {
			return ErrPolicyInvalid
		}
	}
	return nil
}

func mandatoryExcluded(path string, policy RuntimePolicy) bool {
	path = strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
	if path == "." || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") {
		return true
	}
	for _, root := range policy.RuntimeExclusionRoots {
		root = strings.ToLower(filepath.ToSlash(filepath.Clean(root)))
		if root != "." && root != ".." && !strings.HasPrefix(root, "../") &&
			(path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	for _, pattern := range requiredMandatoryExclusions {
		for candidate := path; candidate != "."; candidate = pathpkg.Dir(candidate) {
			if matched, err := doublestar.Match(strings.ToLower(pattern), candidate); err == nil && matched {
				return true
			}
		}
	}
	return false
}

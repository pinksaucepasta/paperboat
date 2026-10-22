package config

import (
	"errors"

	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// WorkspacePolicy bounds workspace policy files (PLAN F18); the same for every plan.
type WorkspacePolicy struct {
	MaxBytes          int `json:"max_bytes"`
	MaxRules          int `json:"max_rules"`
	MaxTags           int `json:"max_tags"`
	MaxGroups         int `json:"max_groups"`
	MaxEntriesPerList int `json:"max_entries_per_list"`
	MaxTests          int `json:"max_tests"`
	VersionsKept      int `json:"versions_kept"`
	ExplainMaxRows    int `json:"explain_max_rows"`
}

func defaultWorkspacePolicy() WorkspacePolicy {
	return WorkspacePolicy{MaxBytes: 256 << 10, MaxRules: 1000, MaxTags: 500, MaxGroups: 200, MaxEntriesPerList: 500, MaxTests: 500, VersionsKept: 100, ExplainMaxRows: 1000}
}

// Limits is the shared/policy form of the file limits.
func (p WorkspacePolicy) Limits() policy.Limits {
	return policy.Limits{MaxBytes: p.MaxBytes, MaxRules: p.MaxRules, MaxTags: p.MaxTags, MaxGroups: p.MaxGroups, MaxEntriesPerList: p.MaxEntriesPerList, MaxTests: p.MaxTests}
}

func (p WorkspacePolicy) validate() error {
	for _, n := range []int{p.MaxBytes, p.MaxRules, p.MaxTags, p.MaxGroups, p.MaxEntriesPerList, p.MaxTests, p.VersionsKept, p.ExplainMaxRows} {
		if n < 1 {
			return errors.New("workspace_policy limits must all be positive")
		}
	}
	return nil
}

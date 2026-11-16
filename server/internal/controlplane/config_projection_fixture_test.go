package controlplane

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// A digest explicitly provided by fixtures, never a production source fallback.
const testConfigurationRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func assignConfigTestProjection(s *ConfigAssignmentService, ctx context.Context, user, machine, repository, mode, warning string, version int64) (dbsqlc.ControlConfigAssignment, error) {
	return s.ConfigureProjection(ctx, user, machine, repository, repository, mode, false, warning, version, nil, nil, testConfigurationRevision)
}
func assignConfigTestTargets(s *ConfigAssignmentService, ctx context.Context, user, machine, pull, push, mode string, automatic bool, warning string, version int64) (dbsqlc.ControlConfigAssignment, error) {
	return s.ConfigureProjection(ctx, user, machine, pull, push, mode, automatic, warning, version, nil, nil, testConfigurationRevision)
}

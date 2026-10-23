package controlplane

import (
	"context"
	"errors"
	"sort"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type DiagnosticsService struct {
	store *db.DB
}

var diagnosticMetricNames = []string{
	"billing_checkout_uncertain_depth",
	"billing_portal_uncertain_depth",
	"config_sync_active_writer_leases",
	"config_sync_conflict_environments",
	"config_sync_eligible_environments",
	"config_sync_lease_contention_total",
	"config_sync_oldest_pending_resolution_age_seconds",
	"config_sync_pending_paths",
	"config_sync_pending_provider_revocations",
	"config_sync_pending_resolutions",
	"config_sync_reporting_environments",
	"config_sync_uncertain_publications",
	"control_operation_dead_letter_depth",
	"control_operation_oldest_age_seconds",
	"control_operation_queue_depth",
	"control_reconciliation_oldest_age_seconds",
	"control_reconciliation_queue_depth",
	"control_stale_node_depth",
	"paperboat_runtime_update_rollbacks_total",
	"user_machine_availability_drift_depth",
	"user_machine_bootstrap_failure_depth",
	"user_machine_heartbeat_oldest_age_seconds",
	"user_machine_privileged_service_error_depth",
	"user_machine_unsupported_host_scope_depth",
}

func DiagnosticMetricNames() []string {
	return append([]string(nil), diagnosticMetricNames...)
}

func NewDiagnosticsService(store *db.DB) *DiagnosticsService {
	return &DiagnosticsService{store: store}
}

func (s *DiagnosticsService) Metrics(ctx context.Context) (map[string]int64, error) {
	row, err := s.store.Queries().GetControlPlaneQueueMetrics(ctx)
	if err != nil {
		return nil, err
	}
	var checkoutUncertain, portalUncertain int64
	err = s.store.SQL().QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE kind='checkout' AND state='uncertain'),count(*) FILTER (WHERE kind='portal' AND state='uncertain') FROM paperboat.billing_workspace_operations WHERE state IN ('pending','uncertain')`).Scan(&checkoutUncertain, &portalUncertain)
	if err != nil {
		return nil, err
	}
	configSync, err := s.store.Queries().GetConfigSyncMetrics(ctx)
	if err != nil {
		return nil, err
	}
	userMachines, err := s.store.Queries().GetUserMachineRuntimeMetrics(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]int64{
		"control_operation_queue_depth":                     row.OperationDepth,
		"control_operation_oldest_age_seconds":              row.OperationOldestAgeSeconds,
		"control_operation_dead_letter_depth":               row.OperationDeadLetterDepth,
		"control_reconciliation_queue_depth":                row.ReconciliationDepth,
		"control_reconciliation_oldest_age_seconds":         row.ReconciliationOldestAgeSeconds,
		"control_stale_node_depth":                          row.StaleNodeDepth,
		"billing_checkout_uncertain_depth":                  checkoutUncertain,
		"billing_portal_uncertain_depth":                    portalUncertain,
		"config_sync_eligible_environments":                 configSync.EligibleEnvironments,
		"config_sync_reporting_environments":                configSync.ReportingEnvironments,
		"config_sync_conflict_environments":                 configSync.ConflictEnvironments,
		"config_sync_pending_paths":                         configSync.PendingPaths,
		"config_sync_uncertain_publications":                configSync.UncertainPublications,
		"config_sync_active_writer_leases":                  configSync.ActiveWriterLeases,
		"config_sync_lease_contention_total":                configSync.LeaseContentionTotal,
		"config_sync_pending_resolutions":                   configSync.PendingResolutions,
		"config_sync_oldest_pending_resolution_age_seconds": configSync.OldestPendingResolutionAgeSeconds,
		"config_sync_pending_provider_revocations":          configSync.PendingProviderRevocations,
		"user_machine_bootstrap_failure_depth":              userMachines.BootstrapFailureDepth,
		"user_machine_heartbeat_oldest_age_seconds":         userMachines.HeartbeatOldestAgeSeconds,
		"user_machine_availability_drift_depth":             userMachines.AvailabilityDriftDepth,
		"user_machine_privileged_service_error_depth":       userMachines.PrivilegedServiceErrorDepth,
		"user_machine_unsupported_host_scope_depth":         userMachines.UnsupportedHostScopeDepth,
		"paperboat_runtime_update_rollbacks_total":          userMachines.UpdateRollbacksTotal,
	}
	if !validDiagnosticMetricSet(result) {
		return nil, errors.New("control-plane metric schema mismatch")
	}
	return result, nil
}

func validDiagnosticMetricSet(values map[string]int64) bool {
	if len(values) != len(diagnosticMetricNames) {
		return false
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for index := range names {
		if names[index] != diagnosticMetricNames[index] {
			return false
		}
	}
	return true
}

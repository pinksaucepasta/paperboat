package main

import (
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func diagnosticCredentialFailure(err error) string {
	switch {
	case errors.Is(err, config.ErrNoCredentials):
		return "no saved sign-in found in this user profile"
	case errors.Is(err, config.ErrCredentialRequiresInteractiveLogin):
		return "saved sign-in needs migration from an interactive session"
	case errors.Is(err, config.ErrCredentialStoreUnavailable):
		return "saved sign-in unavailable · credential store access failed"
	default:
		return "saved sign-in unavailable · credential read or refresh failed"
	}
}

func configAssignmentEnabled(a api.ConfigAssignment) bool {
	return a.RepositoryID != nil && *a.RepositoryID != "" || a.PullRepositoryID != nil && *a.PullRepositoryID != "" || a.PushRepositoryID != nil && *a.PushRepositoryID != ""
}

func diagnosticConfigSync(service string, assignment *api.ConfigAssignment, err error) string {
	prefix := ""
	if err == nil && assignment != nil {
		if !configAssignmentEnabled(*assignment) {
			if service == "not_installed" {
				return "not enabled"
			}
			prefix = "not enabled · "
		}
		if service == "not_installed" {
			return "service missing · configuration sync is assigned"
		}
	} else if service == "not_installed" {
		return "assignment not checked · service not installed"
	}
	switch service {
	case "active":
		return prefix + "worker running"
	case "installed_inactive":
		return prefix + "installed · not running"
	case "installed_unknown":
		return prefix + "installed · status could not be checked"
	case "unavailable":
		return prefix + "service status unavailable · access or query failed"
	case "invalid":
		return prefix + "service definition invalid"
	default:
		return service
	}
}

func diagnosticWorkloads(report localDoctorReport) string {
	if report.WorkloadCounts != "available" {
		return report.WorkloadCounts
	}
	return fmt.Sprintf("%d sessions · %d processes · %d uploads", report.TrackedSessions, report.ActiveProcesses, report.ActiveUploads)
}

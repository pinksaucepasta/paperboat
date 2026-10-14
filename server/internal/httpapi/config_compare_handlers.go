package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

func configCompareConnection(conflicts *controlplane.ConfigConflictService, machines *usermachines.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		principal, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, 401, "unauthenticated", "Authentication is required.")
			return
		}
		limitEnvironmentJSON(w, r, 8192)
		var input struct {
			AssignmentVersion      int64  `json:"assignment_version"`
			Path                   string `json:"path"`
			ExpectedRemoteRevision string `json:"expected_remote_revision"`
			SourceMachineID        string `json:"source_machine_id"`
			CLIClientSessionID     string `json:"cli_client_session_id"`
		}
		if !decodeStrictJSON(w, r, &input) {
			return
		}
		if principal.Client == nil || input.CLIClientSessionID != principal.Client.SessionID {
			writeError(w, r, 403, "not_found_or_forbidden", "Use this CLI client session for the comparison.")
			return
		}
		binding := mint.ConfigCompareBinding{AssignmentVersion: input.AssignmentVersion, ConfigPath: input.Path, ConflictRevision: r.PathValue("conflict_revision"), ExpectedRemoteRevision: input.ExpectedRemoteRevision}
		assignment, err := conflicts.AuthorizeCompare(personalWorkspaceContext(r.Context()), principal.User.ID, r.PathValue("environment_id"), binding)
		if err != nil {
			writeConfigConflictRequestError(w, r, err, "config_compare_invalid", "config_compare_stale", "Config comparison is unavailable.")
			return
		}
		binding.AssignmentID = assignment
		descriptor, err := machines.ConfigCompareDescriptor(personalWorkspaceContext(r.Context()), principal.User.ID, r.PathValue("environment_id"), input.SourceMachineID, input.CLIClientSessionID, binding)
		if err != nil {
			if errors.Is(err, usermachines.ErrNotFound) {
				writeError(w, r, 404, "not_found_or_forbidden", "Comparison machine was not found.")
			} else if errors.Is(err, usermachines.ErrMachineCapabilityUnavailable) {
				writeError(w, r, 409, "config_compare_host_unavailable", "Connect the configuration host and retry the comparison.")
			} else {
				writeError(w, r, 503, "config_compare_unavailable", "Comparison connection could not be created.", err)
			}
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: descriptor})
	}
}

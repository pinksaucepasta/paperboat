package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat-server/internal/environment"
)

type environmentVaultRecordsAPI interface {
	PutVaultRecords(context.Context, string, environment.VaultLayerCoordinate, environment.VaultRecordsPut) (environment.VaultRecordsPage, error)
	ListVaultRecords(context.Context, string, environment.VaultLayerCoordinate, environment.VaultRecordsQuery) (environment.VaultRecordsPage, error)
	GetVaultRecord(context.Context, string, environment.VaultLayerCoordinate, string) (environment.VaultRecordState, error)
	ListVaultHostRecords(context.Context, string, string, string, string, environment.VaultLayerCoordinate, environment.VaultRecordsQuery) (environment.VaultRecordsPage, error)
}

func writeVaultRecordResult(w http.ResponseWriter, r *http.Request, out any, err error) {
	if errors.Is(err, environment.ErrVaultRecordCursorExpired) {
		writeError(w, r, http.StatusConflict, "record_snapshot_required", "Encrypted ENV history changed or the snapshot expired. Fetch a new complete snapshot before applying changes.")
		return
	}
	if errors.Is(err, environment.ErrLimitExceeded) {
		writeError(w, r, http.StatusConflict, "record_capacity_exceeded", "Encrypted ENV record capacity was reached. No changes committed; reduce the batch or active records, or rotate the scope to retire old record identifiers.")
		return
	}
	writeVaultScopeResult(w, r, out, err)
}
func vaultRecordQuery(w http.ResponseWriter, r *http.Request) (environment.VaultRecordsQuery, bool) {
	q := environment.VaultRecordsQuery{Limit: 200}
	values := r.URL.Query()
	for key, v := range values {
		if len(v) != 1 {
			writeError(w, r, 400, "invalid_record_query", "Use one value for each record query field.")
			return q, false
		}
		switch key {
		case "workspace", "workspace_id", "machine_id", "owner_kind", "owner_id", "source_machine_id", "actor_account_id", "limit", "after_sequence", "through_sequence", "cursor":
		default:
			writeError(w, r, 400, "invalid_record_query", "Unsupported record query field.")
			return q, false
		}
	}
	for _, key := range []string{"after_sequence", "through_sequence"} {
		if values.Has(key) {
			n, err := strconv.ParseUint(values.Get(key), 10, 64)
			if err != nil || n > 9007199254740991 {
				writeError(w, r, 400, "invalid_record_query", "Use a valid record sequence.")
				return q, false
			}
			if key == "after_sequence" {
				q.AfterSequence = n
			} else {
				q.ThroughSequence = &n
			}
		}
	}
	if values.Has("limit") {
		n, err := strconv.Atoi(values.Get("limit"))
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, 400, "invalid_record_query", "Record page limit must be between 1 and 200.")
			return q, false
		}
		q.Limit = n
	}
	q.Cursor = values.Get("cursor")
	if len(q.Cursor) > 4096 {
		writeError(w, r, 400, "invalid_record_query", "Record cursor is invalid.")
		return q, false
	}
	return q, true
}
func registerEnvironmentVaultRecordRoutes(mux *http.ServeMux, service environmentVaultRecordsAPI, verifier machineEndpointProofVerifier, read, write func(http.Handler) http.Handler) {
	handler := func(mutation, one bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			account, ok := vaultScopePrincipal(w, r)
			if !ok {
				return
			}
			ws, ok := vaultLayerWorkspace(w, r)
			if !ok {
				return
			}
			c := environment.VaultLayerCoordinate{WorkspaceID: ws, OwnerKind: r.PathValue("owner_kind"), OwnerID: r.PathValue("owner_id"), MachineID: r.URL.Query().Get("machine_id")}
			if mutation {
				if _, ok := vaultRecordQuery(w, r); !ok {
					return
				}
				limitEnvironmentJSON(w, r, 768<<10)
				var in environment.VaultRecordsPut
				if !decodeStrictJSON(w, r, &in) {
					return
				}
				out, err := service.PutVaultRecords(r.Context(), account, c, in)
				writeVaultRecordResult(w, r, out, err)
				return
			}
			q, ok := vaultRecordQuery(w, r)
			if !ok {
				return
			}
			if one {
				out, err := service.GetVaultRecord(r.Context(), account, c, r.PathValue("record_id"))
				writeVaultRecordResult(w, r, out, err)
			} else {
				out, err := service.ListVaultRecords(r.Context(), account, c, q)
				writeVaultRecordResult(w, r, out, err)
			}
		}
	}
	mux.Handle("GET /v1/environment/scopes/{owner_kind}/{owner_id}/records", read(handler(false, false)))
	mux.Handle("PUT /v1/environment/scopes/{owner_kind}/{owner_id}/records", write(handler(true, false)))
	mux.Handle("GET /v1/environment/scopes/{owner_kind}/{owner_id}/records/{record_id}", read(handler(false, true)))
	// Machine proof binds this exact GET path; query coordinates are reauthorized. Human scope reads use the
	// separate authenticated route; this route never accepts a human actor claim.
	denied := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, 401, "machine_identity_required", "A signed machine identity request is required.")
	})
	machine := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := r.Context().Value(environmentMachinePrincipalKey{}).(environmentMachinePrincipal)
		if !ok || p.MachineID != r.PathValue("machine_id") {
			writeError(w, r, 403, "forbidden", "The machine identity does not authorize this resource.")
			return
		}
		ws, ok := vaultLayerWorkspace(w, r)
		if !ok {
			return
		}
		q, ok := vaultRecordQuery(w, r)
		if !ok {
			return
		}
		values := r.URL.Query()
		c := environment.VaultLayerCoordinate{WorkspaceID: ws, OwnerKind: values.Get("owner_kind"), OwnerID: values.Get("owner_id"), MachineID: values.Get("source_machine_id")}
		out, err := service.ListVaultHostRecords(r.Context(), p.AccountID, p.MachineID, values.Get("actor_account_id"), ws, c, q)
		writeVaultRecordResult(w, r, out, err)
	})
	mux.Handle("GET /v1/environment/layers/{machine_id}/records", environmentEnrollmentMachineOrHuman(verifier, denied, machine))
}

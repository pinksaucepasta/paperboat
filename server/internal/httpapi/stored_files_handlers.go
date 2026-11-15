package httpapi

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/teaminbox"
	wire "github.com/pinksaucepasta/paperboat/shared/storedfiles"
)

func registerStoredSenderRoutes(mux *http.ServeMux, s *teaminbox.Service, read, write func(http.Handler) http.Handler) {
	for _, action := range []string{"recipients", "metadata", "upload-urls", "uploaded"} {
		action := action
		method := http.MethodPost
		guard := write
		if action == "recipients" {
			method = http.MethodGet
			guard = read
		}
		mux.Handle(method+" /v1/team-inbox/requests/{request_id}/stored/"+action, guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			noStore(w)
			account, ok := vaultScopePrincipal(w, r)
			if !ok {
				return
			}
			limitEnvironmentJSON(w, r, wire.MaxMetadataBytes)
			id := r.PathValue("request_id")
			var out any
			var err error
			switch action {
			case "recipients":
				var recipients []wire.Recipient
				recipients, err = s.StoredRecipients(r.Context(), account, id)
				out = map[string]any{"recipients": recipients}
			case "metadata":
				var in wire.Metadata
				if !decodeStrictJSON(w, r, &in) {
					return
				}
				err = s.PutStored(r.Context(), account, id, in)
				out = map[string]bool{"stored": err == nil}
			case "upload-urls":
				var in wire.Range
				if !decodeStrictJSON(w, r, &in) {
					return
				}
				out, err = s.StoredUploadURLs(r.Context(), account, id, in)
			case "uploaded":
				err = s.StoredUploaded(r.Context(), account, id)
				out = map[string]bool{"uploaded": err == nil}
			}
			if err != nil {
				teamInboxError(w, r, err)
				return
			}
			writeJSON(w, 200, SuccessResponse{Data: out})
		})))
	}
}

func registerStoredMachineRoutes(mux *http.ServeMux, identities machineRequestVerifier, s *teaminbox.Service) {
	mux.HandleFunc("GET "+wire.MachinePath, storedMachineHandler(identities, s, "list"))
	mux.HandleFunc("POST "+wire.MachinePath+"/{request_id}/download-urls", storedMachineHandler(identities, s, "download"))
	mux.HandleFunc("POST "+wire.MachinePath+"/{request_id}/received", storedMachineHandler(identities, s, "received"))
}
func storedMachineHandler(identities machineRequestVerifier, s *teaminbox.Service, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		proof, perr := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || len(body) > 4096 || perr != nil || len(proof) == 0 || identities == nil {
			writeError(w, r, 401, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		identity, err := identities.VerifyMachineRequest(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), proof, r.Method, r.URL.Path, body)
		if err != nil {
			writeMachineProofFailure(w, r, err, "unauthenticated", "Current signed runtime identity is required.")
			return
		}
		var out any
		id := r.PathValue("request_id")
		switch action {
		case "list":
			out, err = s.StoredDeliverable(r.Context(), identity.MachineID)
		case "download":
			var in wire.Range
			if !strictBody(body, &in) {
				teamInboxError(w, r, teaminbox.ErrInvalid)
				return
			}
			out, err = s.StoredDownloadURLs(r.Context(), identity.MachineID, id, in)
		case "received":
			var in struct {
				ManifestDigest string `json:"manifest_digest"`
			}
			if !strictBody(body, &in) {
				teamInboxError(w, r, teaminbox.ErrInvalid)
				return
			}
			err = s.StoredReceived(r.Context(), identity.MachineID, id, in.ManifestDigest)
			out = map[string]bool{"received": err == nil}
		}
		if err != nil {
			teamInboxError(w, r, err)
			return
		}
		writeJSON(w, 200, SuccessResponse{Data: out})
	}
}

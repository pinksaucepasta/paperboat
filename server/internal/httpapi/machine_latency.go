package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/machinelatency"
	"github.com/pinksaucepasta/paperboat-server/internal/usermachines"
)

type machineLatencyService interface {
	SaveRelays(context.Context, string, string, []machinelatency.RelaySample) error
	List(context.Context, string) ([]machinelatency.Item, error)
}

type machineLookup interface {
	Get(context.Context, string, string) (usermachines.UserMachine, error)
}

// machineLatencyList shows what the machine measured to the edges and relays it can use.
func machineLatencyList(machines machineLookup, latency machineLatencyService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		machine, err := machines.Get(r.Context(), p.User.ID, r.PathValue("machine_id"))
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		items, err := latency.List(r.Context(), machine.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: map[string]any{"items": items}})
	}
}

// machineLatencyReport stores a machine's relay measurements, sent by its signed-in daemon. Edge
// measurements travel on the machine's runtime attachment instead, where the server can check them
// against the candidates it issued.
func machineLatencyReport(machines machineLookup, latency machineLatencyService) http.HandlerFunc {
	type request struct {
		Relays []machinelatency.RelaySample `json:"relays"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		var in request
		decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Send the relay measurements as JSON.")
			return
		}
		machine, err := machines.Get(r.Context(), p.User.ID, r.PathValue("machine_id"))
		if errors.Is(err, usermachines.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "user_machine_not_found", "Machine was not found.")
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		if !machine.CanManage {
			writeError(w, r, http.StatusForbidden, "forbidden", "This account cannot report for that machine.")
			return
		}
		if err := latency.SaveRelays(r.Context(), p.User.ID, machine.ID, in.Relays); err != nil {
			if errors.Is(err, machinelatency.ErrInvalid) {
				writeError(w, r, http.StatusBadRequest, "invalid_request", "Report between 1 and 64 relays this account can use, with a time from 0 to 10000 ms.")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error.", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

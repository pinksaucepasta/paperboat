package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/revenue"
)

// registerOperatorMoney serves the founders' money page: Stripe, read on the server with the existing key, cached for
// two minutes. Without a Stripe key every route answers 503 money_not_configured, which is not a fault.
func registerOperatorMoney(mux *http.ServeMux, opts Options) {
	s := opts.OperatorMoney
	mux.Handle("GET /v1/admin/money", operatorRoute(opts, moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) {
		days := 30
		if raw := r.URL.Query().Get("days"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return nil, revenue.ErrInvalid
			}
			days = n
		}
		return s.Report(r.Context(), days)
	})))
	mux.Handle("GET /v1/admin/money/lists/{list}", operatorRoute(opts, moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) {
		limit, ok := operatorPageLimit(w, r, 100)
		if !ok {
			return nil, errResponded
		}
		return s.List(r.Context(), r.PathValue("list"), r.URL.Query().Get("after"), limit)
	})))
	mux.Handle("GET /v1/admin/money/events", operatorRoute(opts, moneyRead(func(w http.ResponseWriter, r *http.Request) (any, error) {
		limit, ok := operatorPageLimit(w, r, 100)
		if !ok {
			return nil, errResponded
		}
		return s.Events(r.Context(), r.URL.Query().Get("cursor"), limit)
	})))
}

var errResponded = errors.New("response already written")

func moneyRead(read func(http.ResponseWriter, *http.Request) (any, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := read(w, r)
		switch {
		case err == nil:
			writeJSON(w, 200, SuccessResponse{Data: out})
		case errors.Is(err, errResponded):
		case errors.Is(err, revenue.ErrNotConfigured):
			writeError(w, r, 503, "money_not_configured", "Stripe is not configured on this server.")
		case errors.Is(err, revenue.ErrInvalid):
			writeError(w, r, 400, "invalid_request", "Use days=7 or 30, a known list and a valid page.")
		default:
			writeError(w, r, 503, "money_unavailable", "Stripe did not answer. Try again in a minute.", err)
		}
	})
}

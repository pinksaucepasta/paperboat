package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/server/internal/domaininventory"
)

// domainInventoryList is GET /v1/domains: the selected workspace's tunnel and live preview domains in hostname
// order, keyset paged, with the total (Lane C PQ-04 C2).
func domainInventoryList(service *domaininventory.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "Authentication is required.")
			return
		}
		bad := func() {
			writeError(w, r, 400, "invalid_inventory_query", "Use one valid limit (1–200), a cursor from next_cursor, q and kind (tunnel or preview).")
		}
		values := r.URL.Query()
		for key, value := range values {
			if len(value) > 1 {
				bad()
				return
			}
			switch key {
			case "limit", "cursor", "q", "kind", "workspace":
			default:
				bad()
				return
			}
		}
		opts := domaininventory.Options{Limit: 50, Q: values.Get("q"), Kind: values.Get("kind")}
		if raw := values.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 200 {
				bad()
				return
			}
			opts.Limit = n
		}
		if !utf8.ValidString(opts.Q) || len(opts.Q) > 253 || strings.ContainsRune(opts.Q, '\x00') || (opts.Kind != "" && opts.Kind != "tunnel" && opts.Kind != "preview") {
			bad()
			return
		}
		if cursor := values.Get("cursor"); cursor != "" {
			position, err := decodeDomainCursor(cursor)
			if err != nil {
				bad()
				return
			}
			opts.After = &position
		}
		page, err := service.List(personalWorkspaceContext(r.Context()), p.User.ID, opts)
		if errors.Is(err, domaininventory.ErrInvalidInput) {
			bad()
			return
		}
		if err != nil {
			writeError(w, r, 500, "internal_error", "Domains could not be listed.", err)
			return
		}
		data := map[string]any{"items": page.Items, "total": page.Total}
		if page.More && len(page.Items) > 0 {
			next := page.Next()
			raw, _ := json.Marshal([2]string{next.Key, next.ID})
			data["next_cursor"] = base64.RawURLEncoding.EncodeToString(raw)
		}
		writeJSON(w, http.StatusOK, SuccessResponse{Data: data})
	}
}

func decodeDomainCursor(cursor string) (domaininventory.Position, error) {
	if len(cursor) > 1024 {
		return domaininventory.Position{}, errors.New("cursor too long")
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return domaininventory.Position{}, err
	}
	var pair [2]string
	if err := json.Unmarshal(raw, &pair); err != nil || pair[1] == "" || !utf8.ValidString(pair[0]) {
		return domaininventory.Position{}, errors.New("cursor is invalid")
	}
	return domaininventory.Position{Key: pair[0], ID: pair[1]}, nil
}

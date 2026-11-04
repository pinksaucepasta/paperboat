package selfhost

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type InventoryOptions struct {
	Limit, Offset                  int
	Q, Capability, EnrollmentState string
	Ready                          *bool
	// Newest orders the newest installation first (sort=newest); the default order is by name.
	Newest bool
	// After continues the list after the row a previous page'"'"'s next_cursor named; used instead of Offset.
	After string
}
type InventoryPagination struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	Total      int  `json:"total"`
	NextOffset *int `json:"next_offset"`
	// NextCursor is the keyset position of the page after this one, in the list's sort order.
	NextCursor string `json:"next_cursor,omitempty"`
}
type InventoryCounts struct {
	Ready    int `json:"ready"`
	Enrolled int `json:"enrolled"`
	Pending  int `json:"pending"`
}
type InstallationInventory struct {
	Installations []Installation      `json:"installations"`
	Pagination    InventoryPagination `json:"pagination"`
	Counts        InventoryCounts     `json:"counts"`
}

func (o InventoryOptions) normalized() (InventoryOptions, error) {
	if o.Limit == 0 {
		o.Limit = 50
	}
	if o.Limit < 1 || o.Limit > 200 || o.Offset < 0 || o.Offset > 2147483647 || !utf8.ValidString(o.Q) || len(o.Q) > 160 || strings.ContainsRune(o.Q, '\x00') || (o.Capability != "" && o.Capability != "relay" && o.Capability != "tunnel") || (o.EnrollmentState != "" && o.EnrollmentState != "pending" && o.EnrollmentState != "enrolled") || (o.After != "" && o.Offset != 0) || len(o.After) > 512 {
		return o, ErrInvalid
	}
	return o, nil
}

const installationInventorySQL = `WITH visible AS (
 SELECT i.id,i.node_id,coalesce(n.endpoint_host,'') AS endpoint_host,i.name,i.capability,i.scope_kind,i.scope_id,i.claim_complete,i.created_at,
 ` + installationReadySQL + ` AS ready
 FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
 WHERE i.state='active' AND ($1 OR paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$2))
 AND (NOT $3 OR(i.scope_kind=$4 AND i.scope_id=$5))
), filtered AS (
 SELECT * FROM visible WHERE ($6='' OR position(lower($6) in lower(name || ' ' || endpoint_host || ' ' || id || ' ' || node_id))>0)
 AND ($7='' OR capability=$7) AND ($8='' OR claim_complete=($8='enrolled'))
 AND ($9::boolean IS NULL OR ready=$9)
) `

// ListScopedInventory authorizes before discovery and counts and reads the page
// in one consistent snapshot. Public discovery is never admission-limited.
func (s *Service) ListScopedInventory(ctx context.Context, actor string, scope Scope, operator bool, options InventoryOptions) (result InstallationInventory, returnErr error) {
	out := InstallationInventory{Installations: []Installation{}}
	options, err := options.normalized()
	if err != nil {
		return out, err
	}
	if actor == "" || (scope.Kind == "" && scope.ID != "") {
		return out, ErrUnauthorized
	}
	filtered := scope.Kind != ""
	if filtered {
		scope, err = s.authorizeScope(ctx, actor, scope, false, operator)
		if err != nil {
			return out, err
		}
	}
	tx, err := s.database.SQL().BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { returnErr = withTransactionRollbackFailure("list_inventory", returnErr, tx.Rollback()) }()
	args := []any{operator, actor, filtered, scope.Kind, scope.ID, options.Q, options.Capability, options.EnrollmentState, options.Ready}
	out.Pagination = InventoryPagination{Limit: options.Limit, Offset: options.Offset}
	err = tx.QueryRowContext(ctx, installationInventorySQL+`SELECT count(*),count(*) FILTER(WHERE ready),count(*) FILTER(WHERE claim_complete),count(*) FILTER(WHERE NOT claim_complete) FROM filtered`, args...).Scan(&out.Pagination.Total, &out.Counts.Ready, &out.Counts.Enrolled, &out.Counts.Pending)
	if err != nil {
		return out, err
	}
	order, keyed := "name,capability,id", `($12::text IS NULL OR (name,capability,id)>($12,$13::text,$14::text))`
	if options.Newest {
		order, keyed = "created_at DESC,id DESC", `($12::timestamptz IS NULL OR (created_at,id)<($12,$13::text))`
	}
	after, err := decodeInventoryCursor(options.After, options.Newest)
	argc := 3 - btoi(options.Newest)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, installationInventorySQL+`SELECT id,node_id,endpoint_host,name,capability,scope_kind,scope_id,claim_complete,ready,created_at FROM filtered WHERE `+keyed+` ORDER BY `+order+` LIMIT $10 OFFSET $11`, append(args, append([]any{options.Limit + 1, options.Offset}, after[:argc]...)...)...)
	if err != nil {
		return out, err
	}
	var created []time.Time
	for rows.Next() {
		var i Installation
		var complete bool
		var at time.Time
		if err = rows.Scan(&i.InstallationID, &i.NodeID, &i.EndpointHost, &i.Name, &i.Capability, &i.ScopeKind, &i.ScopeID, &complete, &i.Ready, &at); err != nil {
			return out, errors.Join(err, rows.Close())
		}
		i.EnrollmentState = "pending"
		if complete {
			i.EnrollmentState = "enrolled"
		}
		i.TrustNotice = "Native private traffic remains end-to-end encrypted."
		if i.Capability == "tunnel" {
			i.TrustNotice = "Browser HTTP TLS terminates on this server; origin TLS pass-through remains origin-terminated."
		}
		out.Installations = append(out.Installations, i)
		created = append(created, at)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return out, err
	}
	// The page read one row past the limit: another follows when it is there.
	if len(out.Installations) > options.Limit {
		out.Installations, created = out.Installations[:options.Limit], created[:options.Limit]
		last := out.Installations[options.Limit-1]
		key := []string{last.Name, last.Capability, last.InstallationID}
		if options.Newest {
			key = []string{created[options.Limit-1].UTC().Format(time.RFC3339Nano), last.InstallationID}
		}
		raw, _ := json.Marshal(key)
		out.Pagination.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		if options.After == "" {
			next := options.Offset + options.Limit
			out.Pagination.NextOffset = &next
		}
	}
	return out, tx.Commit()
}

var ErrInstallationNotFound = errors.New("installation not found")

func (s *Service) GetInstallation(ctx context.Context, actor, id string, operator bool) (Installation, error) {
	var i Installation
	if actor == "" {
		return i, ErrUnauthorized
	}
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsRune(id, '\x00') {
		return i, ErrInvalid
	}
	var complete bool
	err := s.database.SQL().QueryRowContext(ctx, `SELECT i.id,i.node_id,coalesce(n.endpoint_host,''),i.name,i.capability,i.scope_kind,i.scope_id,i.claim_complete,
 `+installationReadySQL+`
 FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.id=$1 AND i.state='active' AND (($2 AND i.scope_kind='global') OR (NOT $2 AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$3)))`, id, operator, actor).Scan(&i.InstallationID, &i.NodeID, &i.EndpointHost, &i.Name, &i.Capability, &i.ScopeKind, &i.ScopeID, &complete, &i.Ready)
	if err == sql.ErrNoRows {
		return i, ErrInstallationNotFound
	}
	if err != nil {
		return i, err
	}
	i.EnrollmentState = "pending"
	if complete {
		i.EnrollmentState = "enrolled"
	}
	i.TrustNotice = "Native private traffic remains end-to-end encrypted."
	if i.Capability == "tunnel" {
		i.TrustNotice = "Browser HTTP TLS terminates on this server; origin TLS pass-through remains origin-terminated."
	}
	return i, nil
}

// decodeInventoryCursor returns the three cursor arguments of the list's order (nil for the first page).
func decodeInventoryCursor(cursor string, newest bool) ([3]any, error) {
	var out [3]any
	if cursor == "" {
		return out, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	var key []string
	if err != nil || json.Unmarshal(raw, &key) != nil || len(key) != 3-btoi(newest) {
		return out, ErrInvalid
	}
	for i, part := range key {
		if !utf8.ValidString(part) || strings.ContainsRune(part, 0) {
			return out, ErrInvalid
		}
		out[i] = part
	}
	if newest {
		if _, err := time.Parse(time.RFC3339Nano, key[0]); err != nil {
			return out, ErrInvalid
		}
		out[2] = nil
	}
	return out, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

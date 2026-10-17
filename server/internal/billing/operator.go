package billing

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var (
	ErrOperatorBillingForbidden = errors.New("operator billing permission denied")
	ErrOperatorBillingInvalid   = errors.New("operator billing input is invalid")
	ErrOperatorBillingConflict  = errors.New("operator billing version or idempotency conflict")
	ErrOperatorBillingNotFound  = errors.New("operator billing target not found")
)

var billingCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)
var stripeCurrencyPattern = regexp.MustCompile(`^[a-z]{3}$`)

type StripePrice struct {
	ID, ProductID, Currency, Interval string
	UnitAmount                        int64
	Active, Recurring, Licensed       bool
}

type PriceReader interface {
	GetPrice(context.Context, string) (StripePrice, error)
}

func (c HTTPStripeClient) GetPrice(ctx context.Context, id string) (StripePrice, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return StripePrice{}, ErrOperatorBillingInvalid
	}
	var out struct {
		ID, Object, Currency, Product, Type string
		Active                              bool
		UnitAmount                          *int64 `json:"unit_amount"`
		Recurring                           *struct {
			Interval      string
			UsageType     string `json:"usage_type"`
			IntervalCount int    `json:"interval_count"`
		} `json:"recurring"`
	}
	requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.do(requestCtx, http.MethodGet, "/v1/prices/"+url.PathEscape(id), "", nil, &out); err != nil {
		return StripePrice{}, err
	}
	if out.Type == "one_time" && out.Recurring == nil && out.UnitAmount != nil {
		price := StripePrice{ID: out.ID, ProductID: out.Product, Currency: strings.ToLower(out.Currency), Interval: "one_time", UnitAmount: *out.UnitAmount, Active: out.Active}
		if out.ID == id && out.Object == "price" && validStripePriceStructure(price) {
			return price, nil
		}
		return StripePrice{}, errors.New("stripe one-time price has invalid identity, amount or currency")
	}
	if out.ID != id || out.Object != "price" || out.Type != "recurring" || out.Recurring == nil || out.Recurring.UsageType != "licensed" || out.Recurring.IntervalCount != 1 || out.UnitAmount == nil || *out.UnitAmount < 0 || strings.TrimSpace(out.Product) == "" || strings.TrimSpace(out.Currency) == "" {
		return StripePrice{}, errors.New("stripe price is not a recurring licensed unit price")
	}
	switch out.Recurring.Interval {
	case "month", "year":
	default:
		return StripePrice{}, errors.New("stripe price has invalid recurring interval")
	}
	price := StripePrice{ID: out.ID, ProductID: out.Product, Currency: strings.ToLower(out.Currency), Interval: out.Recurring.Interval, UnitAmount: *out.UnitAmount, Active: out.Active, Recurring: true, Licensed: true}
	if !validStripePriceStructure(price) {
		return StripePrice{}, errors.New("stripe price has invalid amount, currency, or interval")
	}
	return price, nil
}

type ProductInput struct {
	Version         int64  `json:"version"`
	DisplayName     string `json:"display_name"`
	ProviderPriceID string `json:"provider_price_id"`
	CatalogType     string `json:"catalog_type"`
	Active          bool   `json:"active"`
	AllowanceBytes  *int64 `json:"allowance_bytes"`
	BytesAmount     *int64 `json:"bytes_amount"`
}

type Product struct {
	Code              string `json:"code"`
	DisplayName       string `json:"display_name"`
	CatalogType       string `json:"catalog_type"`
	ProviderPriceID   string `json:"price_id"`
	ProviderProductID string `json:"product_id"`
	UnitAmount        int64  `json:"unit_amount"`
	PriceVerified     bool   `json:"price_verified"`
	Currency          string `json:"currency"`
	Interval          string `json:"interval"`
	AllowanceBytes    *int64 `json:"allowance_bytes"`
	BytesAmount       *int64 `json:"bytes_amount"`
	Active            bool   `json:"active"`
	Version           int64  `json:"version"`
}

type ProductPage struct {
	Products []Product `json:"products"`
	Next     string    `json:"next,omitempty"`
}

type OperatorPolicy struct {
	PersonalAllowanceBytes *int64 `json:"personal_allowance_bytes"`
	TeamAllowanceBytes     *int64 `json:"team_allowance_bytes"`
	PersonalMachineLimit   *int32 `json:"personal_machine_limit"`
	Revision               int64  `json:"revision"`
}

type OperatorService struct {
	store  *db.DB
	prices PriceReader
	audit  *audit.Writer
}

func NewOperatorService(store *db.DB, prices PriceReader) *OperatorService {
	return &OperatorService{store: store, prices: prices, audit: audit.NewWriter(store)}
}

func (s *OperatorService) ListProducts(ctx context.Context, actorID, after string, limit int) (ProductPage, error) {
	if len(after) > 128 || (after != "" && !billingCodePattern.MatchString(after)) {
		return ProductPage{}, ErrOperatorBillingInvalid
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	var out ProductPage
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		out = ProductPage{Products: []Product{}}
		if err := requireBillingRole(ctx, tx, actorID, false); err != nil {
			return err
		}
		if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorUser, EventType: "billing.operator.products_read", ResourceType: "billing_configuration", ResourceID: "products", Metadata: map[string]any{"after": after, "limit": limit}}); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT code,catalog_ref,catalog_type,provider_price_id,provider_product_id,verified_unit_amount,verified_currency,verified_interval,active,version FROM billing_products WHERE provider='stripe' AND code>$1 ORDER BY code LIMIT $2`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Product
			var metadata string
			var amount sql.NullInt64
			var currency, interval sql.NullString
			if err = rows.Scan(&p.Code, &metadata, &p.CatalogType, &p.ProviderPriceID, &p.ProviderProductID, &amount, &currency, &interval, &p.Active, &p.Version); err != nil {
				return err
			}
			if err = parseProductMetadata(metadata, &p); err != nil {
				return err
			}
			if p.DisplayName == "" {
				p.DisplayName = p.Code
			}
			p.UnitAmount = amount.Int64
			p.PriceVerified = amount.Valid && currency.Valid && interval.Valid
			p.Currency = currency.String
			p.Interval = interval.String
			out.Products = append(out.Products, p)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(out.Products) > limit {
			out.Next = out.Products[limit-1].Code
			out.Products = out.Products[:limit]
		}
		return nil
	})
	return out, err
}

func (s *OperatorService) PutProduct(ctx context.Context, actorID, key, code, reason string, input ProductInput) (Product, error) {
	var out Product
	if s.prices == nil {
		return out, ErrStripeUnavailable
	}
	if strings.TrimSpace(key) == "" || !billingCodePattern.MatchString(code) || strings.TrimSpace(reason) == "" || len(reason) > 500 || validateProductInput(input) != nil {
		return out, ErrOperatorBillingInvalid
	}
	hash, err := operatorHash(struct {
		Code, Reason string
		Input        ProductInput
	}{code, reason, input})
	if err != nil {
		return out, err
	}
	if found, replayErr := s.productReplay(ctx, actorID, key, hash, &out); found || replayErr != nil {
		return out, replayErr
	}
	price, err := s.prices.GetPrice(ctx, input.ProviderPriceID)
	if err != nil {
		return out, fmt.Errorf("verify Stripe price: %w", err)
	}
	if !validStripePriceStructure(price) || (input.CatalogType == "top_up") == price.Recurring || price.ID != input.ProviderPriceID || (input.Active && !price.Active) {
		return out, ErrOperatorBillingInvalid
	}
	metadata, _ := json.Marshal(struct {
		DisplayName    string `json:"display_name"`
		AllowanceBytes *int64 `json:"allowance_bytes,omitempty"`
		BytesAmount    *int64 `json:"bytes_amount,omitempty"`
	}{strings.TrimSpace(input.DisplayName), input.AllowanceBytes, input.BytesAmount})
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireBillingRole(ctx, tx, actorID, true); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "billing-product:"+code).Scan(new(any)); err != nil {
			return err
		}
		if found, err := operationReplayTx(ctx, tx, key, hash, &out); found || err != nil {
			return err
		}
		if input.Version == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO billing_products(id,code,provider,provider_product_id,provider_price_id,catalog_type,catalog_ref,active,verified_unit_amount,verified_currency,verified_interval) VALUES($1,$2,'stripe',$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING RETURNING version`, newBillingID(), code, price.ProductID, price.ID, input.CatalogType, string(metadata), input.Active, price.UnitAmount, price.Currency, price.Interval).Scan(&out.Version)
		} else {
			err = tx.QueryRow(ctx, `UPDATE billing_products SET catalog_ref=$4,active=$5,verified_unit_amount=$6,verified_currency=$7,verified_interval=$8,updated_at=now(),version=version+1 WHERE code=$1 AND version=$2 AND catalog_type=$3 AND provider='stripe' AND provider_price_id=$9 AND provider_product_id=$10 RETURNING version`, code, input.Version, input.CatalogType, string(metadata), input.Active, price.UnitAmount, price.Currency, price.Interval, price.ID, price.ProductID).Scan(&out.Version)
		}
		if err == pgx.ErrNoRows {
			return ErrOperatorBillingConflict
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrOperatorBillingConflict
			}
			return err
		}
		out = Product{Code: code, DisplayName: strings.TrimSpace(input.DisplayName), CatalogType: input.CatalogType, ProviderPriceID: price.ID, ProviderProductID: price.ProductID, UnitAmount: price.UnitAmount, PriceVerified: true, Currency: price.Currency, Interval: price.Interval, AllowanceBytes: input.AllowanceBytes, BytesAmount: input.BytesAmount, Active: input.Active, Version: out.Version}
		return recordOperatorResult(ctx, tx, s.audit, actorID, key, "put_product", code, hash, reason, out)
	})
	return out, err
}

func (s *OperatorService) productReplay(ctx context.Context, actor, key, hash string, out *Product) (bool, error) {
	var found bool
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireBillingRole(ctx, tx, actor, true); err != nil {
			return err
		}
		var e error
		found, e = operationReplayTx(ctx, tx, key, hash, out)
		return e
	})
	return found, err
}

func (s *OperatorService) Policy(ctx context.Context, actorID string) (OperatorPolicy, error) {
	var out OperatorPolicy
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireBillingRole(ctx, tx, actorID, false); err != nil {
			return err
		}
		if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actorID, ActorType: audit.ActorUser, EventType: "billing.operator.policy_read", ResourceType: "billing_configuration", ResourceID: "global", Metadata: map[string]any{}}); err != nil {
			return err
		}
		return scanPolicy(tx.QueryRow(ctx, `SELECT personal_allowance_bytes,team_allowance_bytes,personal_machine_limit,revision FROM billing_operator_policy WHERE singleton`), &out)
	})
	return out, err
}

func (s *OperatorService) PutPolicy(ctx context.Context, actorID, key, reason string, input OperatorPolicy) (OperatorPolicy, error) {
	var out OperatorPolicy
	if strings.TrimSpace(key) == "" || strings.TrimSpace(reason) == "" || len(reason) > 500 || input.Revision < 1 || invalidPolicy(input) {
		return out, ErrOperatorBillingInvalid
	}
	hash, err := operatorHash(struct {
		Reason string
		Input  OperatorPolicy
	}{reason, input})
	if err != nil {
		return out, err
	}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := requireBillingRole(ctx, tx, actorID, true); err != nil {
			return err
		}
		if found, err := operationReplayTx(ctx, tx, key, hash, &out); found || err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE billing_operator_policy SET personal_allowance_bytes=$1,team_allowance_bytes=$2,personal_machine_limit=$3,revision=revision+1,updated_at=now() WHERE singleton AND revision=$4 RETURNING personal_allowance_bytes,team_allowance_bytes,personal_machine_limit,revision`, input.PersonalAllowanceBytes, input.TeamAllowanceBytes, input.PersonalMachineLimit, input.Revision).Scan(nullableInt64(&out.PersonalAllowanceBytes), nullableInt64(&out.TeamAllowanceBytes), nullableInt32(&out.PersonalMachineLimit), &out.Revision)
		if err == pgx.ErrNoRows {
			return ErrOperatorBillingConflict
		}
		if err != nil {
			return err
		}
		return recordOperatorResult(ctx, tx, s.audit, actorID, key, "put_policy", "global", hash, reason, out)
	})
	return out, err
}

func requireBillingRole(ctx context.Context, tx *db.Tx, actor string, admin bool) error {
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM operator_accounts WHERE id=$1 AND status='active' FOR SHARE`, actor).Scan(&role)
	if err == pgx.ErrNoRows {
		return ErrOperatorBillingForbidden
	}
	if err != nil {
		return err
	}
	if admin && role != "admin" {
		return ErrOperatorBillingForbidden
	}
	if !admin && role != "admin" && role != "billing" {
		return ErrOperatorBillingForbidden
	}
	return nil
}
func validateProductInput(in ProductInput) error {
	if in.CatalogType == "top_up" {
		if in.BytesAmount == nil || *in.BytesAmount <= 0 || in.AllowanceBytes != nil {
			return ErrOperatorBillingInvalid
		}
	} else if in.BytesAmount != nil {
		return ErrOperatorBillingInvalid
	}
	if in.Version < 0 || strings.TrimSpace(in.DisplayName) == "" || len(in.DisplayName) > 120 || strings.TrimSpace(in.ProviderPriceID) == "" || (in.CatalogType != "personal_subscription" && in.CatalogType != "team_subscription" && in.CatalogType != "top_up") || (in.AllowanceBytes != nil && *in.AllowanceBytes < 0) {
		return ErrOperatorBillingInvalid
	}
	return nil
}
func validStripePriceStructure(p StripePrice) bool {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.ProductID) == "" || p.UnitAmount < 0 || !stripeCurrencyPattern.MatchString(p.Currency) {
		return false
	}
	switch p.Interval {
	case "one_time":
		return !p.Recurring && !p.Licensed && p.UnitAmount > 0
	case "month", "year":
		return p.Recurring && p.Licensed
	}
	return false
}
func invalidPolicy(p OperatorPolicy) bool {
	return (p.PersonalAllowanceBytes != nil && *p.PersonalAllowanceBytes < 0) || (p.TeamAllowanceBytes != nil && *p.TeamAllowanceBytes < 0) || (p.PersonalMachineLimit != nil && *p.PersonalMachineLimit < 0)
}
func operatorHash(v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func operationReplayTx[T any](ctx context.Context, tx *db.Tx, key, hash string, out *T) (bool, error) {
	var stored string
	var result []byte
	err := tx.QueryRow(ctx, `SELECT request_hash,result FROM billing_operator_operations WHERE idempotency_key=$1`, key).Scan(&stored, &result)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored != hash {
		return true, ErrOperatorBillingConflict
	}
	if err = json.Unmarshal(result, out); err != nil {
		return true, err
	}
	return true, nil
}
func recordOperatorResult(ctx context.Context, tx *db.Tx, w *audit.Writer, actor, key, kind, target, hash, reason string, result any) error {
	b, e := json.Marshal(result)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO billing_operator_operations(idempotency_key,actor_user_id,operation_type,target,request_hash,result) VALUES($1,$2,$3,$4,$5,$6)`, key, actor, kind, target, hash, string(b)); e != nil {
		var conflict *pgconn.PgError
		if errors.As(e, &conflict) && conflict.Code == "23505" {
			return ErrOperatorBillingConflict
		}
		return e
	}
	return w.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "billing.operator." + kind, ResourceType: "billing_configuration", ResourceID: target, IdempotencyKey: "billing.operator:" + key, Metadata: map[string]any{"reason": reason}})
}
func parseProductMetadata(raw string, p *Product) error {
	var m struct {
		DisplayName    string `json:"display_name"`
		AllowanceBytes *int64 `json:"allowance_bytes"`
		BytesAmount    *int64 `json:"bytes_amount"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return err
	}
	p.DisplayName = m.DisplayName
	p.AllowanceBytes = m.AllowanceBytes
	p.BytesAmount = m.BytesAmount
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanPolicy(row rowScanner, p *OperatorPolicy) error {
	return row.Scan(nullableInt64(&p.PersonalAllowanceBytes), nullableInt64(&p.TeamAllowanceBytes), nullableInt32(&p.PersonalMachineLimit), &p.Revision)
}
func nullableInt64(dst **int64) sql.Scanner { return &nullableInt64Dest{dst: dst} }
func nullableInt32(dst **int32) sql.Scanner { return &nullableInt32Dest{dst: dst} }

type nullableInt64Dest struct{ dst **int64 }

func (n *nullableInt64Dest) Scan(src any) error {
	var v sql.NullInt64
	if err := v.Scan(src); err != nil {
		return err
	}
	if v.Valid {
		x := v.Int64
		*n.dst = &x
	} else {
		*n.dst = nil
	}
	return nil
}

type nullableInt32Dest struct{ dst **int32 }

func (n *nullableInt32Dest) Scan(src any) error {
	var v sql.NullInt32
	if err := v.Scan(src); err != nil {
		return err
	}
	if v.Valid {
		x := v.Int32
		*n.dst = &x
	} else {
		*n.dst = nil
	}
	return nil
}
func newBillingID() string { return "plan_" + uuid.NewString() }

package revenue

import (
	"context"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Records maps Stripe ids onto the billing tables.
type Records struct{ Store *db.DB }

func (r Records) PlanCodes(ctx context.Context) (map[string]string, error) {
	rows, err := r.Store.Pool().Query(ctx, `SELECT provider_price_id, code FROM billing_products`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var price, code string
		if err := rows.Scan(&price, &code); err != nil {
			return nil, err
		}
		out[price] = code
	}
	return out, rows.Err()
}

func (r Records) Workspaces(ctx context.Context, customers []string) (map[string]string, error) {
	out := map[string]string{}
	if len(customers) == 0 {
		return out, nil
	}
	rows, err := r.Store.Pool().Query(ctx, `SELECT provider_customer_id, id FROM billing_workspaces WHERE provider_customer_id = ANY($1)`, customers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var customer, workspace string
		if err := rows.Scan(&customer, &workspace); err != nil {
			return nil, err
		}
		out[customer] = workspace
	}
	return out, rows.Err()
}

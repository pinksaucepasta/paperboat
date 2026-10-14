package browseraccess

import (
	"context"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machineservices"
)

// A dormant automatic hostname materializes exactly one existing lazy policy,
// after exact resource authorization and before any activation or origin probe.
func ensureMachineServicePolicy(ctx context.Context, tx *db.Tx, host, account string, now time.Time, domain string) error {
	if domain == "" || !strings.HasSuffix(host, "."+domain) || strings.Count(host, ".") != strings.Count(domain, ".")+1 {
		return ErrNotAuthorized
	}
	var machine, owner, environment string
	var installation int64
	var port int
	e := tx.QueryRow(ctx, `SELECT m.id,m.user_id,m.environment_id,m.installation_generation,n.port FROM managed_hostnames n JOIN user_machines m ON m.id=n.machine_id WHERE n.hostname=$1 AND n.resource_kind='machine' AND m.deleted_at IS NULL AND m.revoked_at IS NULL`, host).Scan(&machine, &owner, &environment, &installation, &port)
	if e != nil {
		return mapMissing(e)
	}
	portText := strconv.Itoa(port)
	if _, _, e = machineservices.ResolveBrowserTx(ctx, tx, account, machine, port); e != nil {
		if e == machineservices.ErrDenied {
			return ErrNotAuthorized
		}
		return e
	}
	if _, e = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,168))`, environment); e != nil {
		return e
	}
	var count int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM lazy_access_policies WHERE environment_id=$1 AND deleted_at IS NULL`, environment).Scan(&count); e != nil {
		return e
	}
	if count >= machineservices.MaxBrowserPolicies {
		return ErrCapacity
	}
	address := net.JoinHostPort("127.0.0.1", portText)
	if e = tx.QueryRow(ctx, `SELECT coalesce((SELECT CASE WHEN service->>'address_family'='ipv6' THEN '[::1]:' ELSE '127.0.0.1:' END||$2 FROM machine_service_snapshots snapshot CROSS JOIN LATERAL jsonb_array_elements(snapshot.services) service WHERE snapshot.machine_id=$1 AND snapshot.installation_generation=$3 AND snapshot.expires_at>now() AND service->>'port'=$2 LIMIT 1),$4)`, machine, portText, installation, address).Scan(&address); e != nil {
		return e
	}
	// The browser's default scheme is HTTP. An explicitly selected existing lazy
	// policy at this hostname remains authoritative, including HTTPS/h2c.
	_, e = tx.Exec(ctx, `INSERT INTO lazy_access_policies(id,hostname,account_id,environment_id,machine_id,installation_generation,generation,target_scheme,target_address,access_mode,ownership_mode,expires_at,authorization_source) VALUES($1,$2,$3,$4,$5,$6,1,'http',$7,'private','persistent_port',$8,'machine_service') ON CONFLICT(hostname) DO NOTHING`, "machine_service_"+machine+"_"+portText, host, owner, environment, machine, installation, address, now.Add(100*365*24*time.Hour))
	return e
}
func applyMachineServicePrincipal(ctx context.Context, tx *db.Tx, a authority, account string) (authority, bool, error) {
	if a.kind != "lazy_policy" {
		return a, false, nil
	}
	var source, address string
	if e := tx.QueryRow(ctx, `SELECT authorization_source,target_address FROM lazy_access_policies WHERE id=$1`, a.id).Scan(&source, &address); e != nil {
		return a, true, mapMissing(e)
	}
	if source != "machine_service" {
		return a, false, nil
	}
	_, portText, e := net.SplitHostPort(address)
	if e != nil {
		return a, true, ErrNotAuthorized
	}
	port, e := strconv.Atoi(portText)
	if e != nil {
		return a, true, ErrNotAuthorized
	}
	selected, decision, e := machineservices.ResolveBrowserTx(ctx, tx, account, a.machine, port)
	if e != nil {
		if e == machineservices.ErrDenied {
			e = ErrNotAuthorized
		}
		return a, true, e
	}
	a.team, a.td = selected.TeamID, decision
	a.previewOwnerSession = selected.Fingerprint()
	return a, true, nil
}

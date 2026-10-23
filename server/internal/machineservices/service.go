// Package machineservices owns exact machine-port policy and short-lived runtime
// announcements. Announcements describe availability; they never confer access.
package machineservices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	wire "github.com/pinksaucepasta/paperboat/shared/machineservices"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/machinealias"
	"github.com/pinksaucepasta/paperboat-server/internal/naming"
)

const MaxMachines = 128

// MaxCandidateMachines bounds the online machines with a service snapshot that one list examines before the
// per-port authority check selects at most MaxMachines of them.
const MaxCandidateMachines = 4 * MaxMachines
const MaxBrowserPolicies = 32
const SnapshotLifetime = 30 * time.Second

var ErrDenied = errors.New("machine service access denied")

// ErrTeamPrivateAccessOff is a denial with a reason the caller can act on: the machine is shared into
// the caller's team with a private_access grant, but the team does not allow private access yet.
var ErrTeamPrivateAccessOff = errors.New("team private access is off")
var ErrConflict = errors.New("machine service generation conflict")

type Authority struct {
	SourceKind, SourceID, TeamID string
	Generation                   int64
}
type Service struct {
	LocalAccessBaseDomain string
	DB                    *sql.DB
	Now                   func() time.Time
}

// serviceCleanupFailure keeps an operation failure and its transaction/row
// cleanup failure discoverable without exposing either error's text.
type serviceCleanupFailure struct {
	operation error
	cleanup   error
}

func (*serviceCleanupFailure) Error() string { return "machine service cleanup failed" }

func (e *serviceCleanupFailure) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.operation != nil {
		causes = append(causes, e.operation)
	}
	if e.cleanup != nil {
		causes = append(causes, e.cleanup)
	}
	return causes
}

func withServiceCleanupFailure(operation, cleanup error) error {
	if cleanup == nil {
		return operation
	}
	return &serviceCleanupFailure{operation: operation, cleanup: cleanup}
}

func withServiceRollbackFailure(operation, rollback error) error {
	if rollback == sql.ErrTxDone {
		return operation
	}
	return withServiceCleanupFailure(operation, rollback)
}

func captureServiceRollback(tx *sql.Tx, result *error) {
	if err := tx.Rollback(); err != nil {
		*result = withServiceRollbackFailure(*result, err)
	}
}

func New(database *db.DB, localAccessBaseDomain string) *Service {
	return &Service{DB: database.SQL(), Now: time.Now, LocalAccessBaseDomain: localAccessBaseDomain}
}
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// ResolveTx selects explicit authority first. Multiple team paths are ambiguous;
// callers must not silently choose a tenant. No runtime or origin is consulted.
func ResolveTx(ctx context.Context, tx *sql.Tx, actor, machine string, port int) (authority Authority, resultErr error) {
	rows, e := tx.QueryContext(ctx, `SELECT source_kind,source_id,source_generation,coalesce(team_id,'') FROM paperboat.machine_service_authorities($1,$2,$3) ORDER BY CASE WHEN team_id IS NULL THEN 0 ELSE 1 END,CASE WHEN source_kind='automatic' THEN 1 ELSE 0 END,source_kind,source_id,team_id`, actor, machine, port)
	if e != nil {
		return Authority{}, e
	}
	defer func() {
		resultErr = withServiceCleanupFailure(resultErr, rows.Close())
	}()
	var out Authority
	seen := false
	for rows.Next() {
		var a Authority
		if e = rows.Scan(&a.SourceKind, &a.SourceID, &a.Generation, &a.TeamID); e != nil {
			return Authority{}, e
		}
		if !seen {
			out = a
			seen = true
		}
		if out.TeamID != "" && a.TeamID != out.TeamID {
			return Authority{}, ErrDenied
		}
	}
	if e = rows.Err(); e != nil {
		return Authority{}, e
	}
	if !seen {
		return Authority{}, ErrDenied
	}
	return out, nil
}

// TeamPrivateAccessOffTx reports whether the actor holds a private_access grant on the machine through
// a team that has not turned private access on: the only thing between them and the ports.
func TeamPrivateAccessOffTx(ctx context.Context, tx *sql.Tx, actor, machine string) (bool, error) {
	var off bool
	e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.team_resource_bindings b JOIN paperboat.workspace_policies p ON p.team_id=b.team_id AND NOT p.allow_private_access
 WHERE b.resource_kind='machine' AND b.resource_id=$2 AND b.active AND paperboat.team_machine_bound_for($1,b.team_id,$2)
 AND NOT isempty(paperboat.workspace_policy_rule_ports($1,b.team_id,$2,'private_access')))`, actor, machine).Scan(&off)
	return off, e
}
func (s *Service) GetPolicy(ctx context.Context, actor, machine string) (wire.Policy, error) {
	p := wire.Policy{Schema: "paperboat.machine-service-policy/v1", MachineID: machine}
	e := s.DB.QueryRowContext(ctx, `SELECT machine_service_policy_generation,personal_auto_enabled FROM paperboat.user_machines WHERE id=$1 AND paperboat.machine_management_allowed($2,id)`, machine, actor).Scan(&p.Generation, &p.PersonalAutoEnabled)
	if machineServiceRowMissing(e) {
		e = ErrDenied
	}
	return p, e
}
func (s *Service) UpdatePolicy(ctx context.Context, actor, machine string, in wire.PolicyUpdate) (policy wire.Policy, resultErr error) {
	if in.ExpectedGeneration < 1 {
		return wire.Policy{}, wire.ErrInvalid
	}
	tx, e := db.BeginSerializable(ctx, s.DB)
	if e != nil {
		return wire.Policy{}, e
	}
	defer captureServiceRollback(tx, &resultErr)
	var gen int64
	e = tx.QueryRowContext(ctx, `SELECT machine_service_policy_generation FROM paperboat.user_machines WHERE id=$1 AND paperboat.machine_management_allowed($2,id) FOR UPDATE`, machine, actor).Scan(&gen)
	if machineServiceRowMissing(e) {
		return wire.Policy{}, ErrDenied
	}
	if e != nil {
		return wire.Policy{}, e
	}
	if gen != in.ExpectedGeneration {
		return wire.Policy{}, ErrConflict
	}
	p := wire.Policy{Schema: "paperboat.machine-service-policy/v1", MachineID: machine, Generation: gen + 1, PersonalAutoEnabled: in.PersonalAutoEnabled}
	_, e = tx.ExecContext(ctx, `UPDATE paperboat.user_machines SET personal_auto_enabled=$2,machine_service_policy_generation=$3 WHERE id=$1`, machine, in.PersonalAutoEnabled, p.Generation)
	if e != nil {
		return wire.Policy{}, e
	}
	_, e = tx.ExecContext(ctx, `UPDATE paperboat.user_machine_access_sessions SET state='revoked',revoked_at=now(),updated_at=now(),revocation_reason='machine_service_policy_changed' WHERE user_machine_id=$1 AND state='active' AND machine_service_source_kind='automatic'`, machine)
	if e != nil {
		return wire.Policy{}, e
	}
	return p, tx.Commit()
}
func (s *Service) RuntimePolicy(ctx context.Context, environment, machine string) (policy wire.RuntimePolicy, resultErr error) {
	p := wire.RuntimePolicy{ExplicitPorts: []wire.Port{}}
	// Serialize projection for this machine. Audience and explicit-route changes
	// have their own authority generations, independent of the machine toggle.
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return p, e
	}
	defer captureServiceRollback(tx, &resultErr)
	e = tx.QueryRowContext(ctx, `SELECT (owner_team_id IS NULL AND personal_auto_enabled) OR EXISTS(SELECT 1 FROM paperboat.team_resource_bindings b JOIN paperboat.team_members member ON member.team_id=b.team_id AND member.active WHERE b.resource_kind='machine' AND b.resource_id=m.id AND b.active AND paperboat.team_machine_capability_allowed(member.account_id,b.team_id,m.id,'private_access')) FROM paperboat.user_machines m WHERE id=$1 AND environment_id=$2 AND deleted_at IS NULL AND revoked_at IS NULL FOR UPDATE`, machine, environment).Scan(&p.AutomaticDiscoveryEnabled)
	if machineServiceRowMissing(e) {
		return p, ErrDenied
	}
	if e != nil {
		return p, e
	}
	rows, e := tx.QueryContext(ctx, `SELECT target_address FROM paperboat.lazy_access_policies WHERE machine_id=$1 AND environment_id=$2 AND authorization_source='explicit' AND deleted_at IS NULL AND expires_at>now() UNION SELECT r.origin_address FROM paperboat.tunnels t JOIN paperboat.tunnel_routes r ON r.tunnel_id=t.id WHERE t.created_by_host_id=$1 AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now()) AND r.protocol='private_tcp' AND r.desired_state='active' AND r.deleted_at IS NULL`, machine, environment)
	if e != nil {
		return p, e
	}
	rowsClosed := false
	defer func() {
		if !rowsClosed {
			resultErr = withServiceCleanupFailure(resultErr, rows.Close())
		}
	}()
	for rows.Next() {
		var a string
		if e = rows.Scan(&a); e != nil {
			return p, e
		}
		h, n, e := net.SplitHostPort(a)
		if e != nil {
			continue
		}
		port, e := strconv.Atoi(n)
		if e != nil {
			continue
		}
		v := wire.Port{Port: port, AddressFamily: "ipv4"}
		if h == "::1" {
			v.AddressFamily = "ipv6"
		} else if h != "127.0.0.1" {
			continue
		}
		if wire.ValidPort(v) && !slices.Contains(p.ExplicitPorts, v) {
			p.ExplicitPorts = append(p.ExplicitPorts, v)
		}
	}
	if e = rows.Err(); e != nil {
		return p, e
	}
	slices.SortFunc(p.ExplicitPorts, func(a, b wire.Port) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		return strings.Compare(a.AddressFamily, b.AddressFamily)
	})
	if len(p.ExplicitPorts) > wire.MaxServices {
		p.ExplicitPorts = p.ExplicitPorts[:wire.MaxServices]
	}
	if e = rows.Close(); e != nil {
		rowsClosed = true
		return p, e
	}
	rowsClosed = true
	encoded, e := json.Marshal(p.ExplicitPorts)
	if e != nil {
		return p, e
	}
	e = tx.QueryRowContext(ctx, `INSERT INTO paperboat.machine_service_discovery_policies(machine_id,generation,automatic_discovery_enabled,explicit_ports) VALUES($1,1,$2,$3)
 ON CONFLICT(machine_id) DO UPDATE SET
 generation=machine_service_discovery_policies.generation+CASE WHEN machine_service_discovery_policies.automatic_discovery_enabled IS DISTINCT FROM EXCLUDED.automatic_discovery_enabled OR machine_service_discovery_policies.explicit_ports IS DISTINCT FROM EXCLUDED.explicit_ports THEN 1 ELSE 0 END,
 automatic_discovery_enabled=EXCLUDED.automatic_discovery_enabled,explicit_ports=EXCLUDED.explicit_ports
 RETURNING generation`, machine, p.AutomaticDiscoveryEnabled, encoded).Scan(&p.Generation)
	if e != nil {
		return p, e
	}
	return p, tx.Commit()
}
func (s *Service) RecordMachineServices(ctx context.Context, environment, machine string, in wire.Snapshot, observedAt time.Time) (resultErr error) {
	now := s.now()
	if observedAt.IsZero() || observedAt.After(now.Add(5*time.Second)) || !observedAt.Add(SnapshotLifetime).After(now) {
		return wire.ErrInvalid
	}
	expiry := observedAt.Add(SnapshotLifetime)
	if expiry.After(now.Add(SnapshotLifetime)) {
		expiry = now.Add(SnapshotLifetime)
	}
	if e := wire.ValidateSnapshot(in, now); e != nil {
		return e
	}
	policy, e := s.RuntimePolicy(ctx, environment, machine)
	if e != nil {
		return e
	}
	for _, p := range in.Services {
		if !(p.Port >= 1024 && policy.AutomaticDiscoveryEnabled) && !slices.Contains(policy.ExplicitPorts, p) {
			return ErrDenied
		}
	}
	data, e := json.Marshal(in.Services)
	if e != nil {
		return e
	}
	if in.Services == nil {
		data = []byte("[]")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer captureServiceRollback(tx, &resultErr)
	result, e := tx.ExecContext(ctx, `INSERT INTO paperboat.machine_service_snapshots(machine_id,installation_generation,boot_id,started_at,generation,services,expires_at,updated_at)
 SELECT id,$3,$4,$5,$6,$7,$8,$9 FROM paperboat.user_machines WHERE id=$1 AND environment_id=$2 AND installation_generation=$3 AND revoked_at IS NULL AND deleted_at IS NULL
 ON CONFLICT(machine_id) DO UPDATE SET installation_generation=EXCLUDED.installation_generation,boot_id=EXCLUDED.boot_id,started_at=EXCLUDED.started_at,generation=EXCLUDED.generation,services=EXCLUDED.services,expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
 WHERE EXCLUDED.installation_generation>machine_service_snapshots.installation_generation OR (EXCLUDED.installation_generation=machine_service_snapshots.installation_generation AND ((EXCLUDED.boot_id=machine_service_snapshots.boot_id AND EXCLUDED.started_at=machine_service_snapshots.started_at AND (EXCLUDED.generation>machine_service_snapshots.generation OR (EXCLUDED.generation=machine_service_snapshots.generation AND EXCLUDED.services=machine_service_snapshots.services AND EXCLUDED.expires_at>=machine_service_snapshots.expires_at))) OR (EXCLUDED.boot_id<>machine_service_snapshots.boot_id AND EXCLUDED.started_at>machine_service_snapshots.started_at)))`, machine, environment, in.InstallationGeneration, in.BootID, in.StartedAt, in.Generation, data, expiry, now)
	if e != nil {
		return fmt.Errorf("record machine services: %w", e)
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		return ErrConflict
	}
	// A family replacement changes the exact browser target and therefore its
	// policy generation; explicit port policies retain their selected address.
	_, e = tx.ExecContext(ctx, `UPDATE paperboat.lazy_access_policies p SET target_address=CASE WHEN service->>'address_family'='ipv6' THEN '[::1]:' ELSE '127.0.0.1:' END||(service->>'port'),generation=p.generation+1,updated_at=$3
 FROM jsonb_array_elements($2::jsonb) service
 WHERE p.machine_id=$1 AND p.authorization_source='machine_service' AND p.deleted_at IS NULL
 AND p.target_address IN ('127.0.0.1:'||(service->>'port'),'[::1]:'||(service->>'port'))
 AND p.target_address<>CASE WHEN service->>'address_family'='ipv6' THEN '[::1]:' ELSE '127.0.0.1:' END||(service->>'port')`, machine, data, now)
	if e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Service) List(ctx context.Context, actor string) (items []wire.Item, resultErr error) {
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	defer captureServiceRollback(tx, &resultErr)
	// Candidates are the only machines machine_service_authorities can grant: the actor's own, machines bound to an
	// active team of the actor, and hosts of lazy policies or tunnels bound to such a team. Without them every
	// daemon's 8-second poll evaluated the authorities for every online machine in the database. ResolveTx below
	// decides each port once and drops machines with none; an SQL prefilter on the same authorities doubled the work.
	rows, e := tx.QueryContext(ctx, `WITH teams AS (SELECT tm.team_id FROM paperboat.team_members tm WHERE tm.account_id=$1 AND tm.active),
bound AS (SELECT b.resource_kind,b.resource_id FROM paperboat.team_resource_bindings b JOIN teams USING(team_id) WHERE b.active AND b.resource_kind IN ('machine','lazy_policy','tunnel')),
candidates AS (SELECT m.id FROM paperboat.user_machines m WHERE m.user_id=$1
 UNION SELECT resource_id FROM bound WHERE resource_kind='machine'
 UNION SELECT p.machine_id FROM paperboat.lazy_access_policies p JOIN bound ON bound.resource_kind='lazy_policy' AND bound.resource_id=p.id
 UNION SELECT t.created_by_host_id FROM paperboat.tunnels t JOIN bound ON bound.resource_kind='tunnel' AND bound.resource_id=t.id)
SELECT m.id,m.alias,m.description,s.installation_generation,s.boot_id,m.machine_service_policy_generation,s.generation,s.expires_at,s.services,
 (m.user_id=$1 AND m.owner_team_id IS NULL AND m.personal_auto_enabled AND m.seat_state='occupied' AND m.state NOT IN ('pending','revoked','deleted')
  AND EXISTS(SELECT 1 FROM paperboat.users u WHERE u.id=$1 AND u.status='active')) FROM paperboat.user_machines m JOIN paperboat.machine_service_snapshots s ON s.machine_id=m.id AND s.installation_generation=m.installation_generation WHERE m.id IN (SELECT id FROM candidates) AND s.expires_at>now() AND m.online AND m.state='online' AND m.deleted_at IS NULL AND m.revoked_at IS NULL ORDER BY m.id LIMIT $2`, actor, MaxCandidateMachines+1)
	if e != nil {
		return nil, e
	}
	rowsClosed := false
	defer func() {
		if !rowsClosed {
			resultErr = withServiceCleanupFailure(resultErr, rows.Close())
		}
	}()
	var candidates []wire.Item
	// automaticPersonal[i]: candidate i is the actor's own personal machine with automatic discovery on, so
	// machine_service_authorities' automatic branch allows every port from 1024 without being called.
	var automaticPersonal []bool
	for rows.Next() {
		var i wire.Item
		var data []byte
		var automatic bool
		if e = rows.Scan(&i.MachineID, &i.Alias, &i.Description, &i.InstallationGeneration, &i.BootID, &i.PolicyGeneration, &i.AnnouncementGeneration, &i.ExpiresAt, &data, &automatic); e != nil {
			return nil, e
		}
		automaticPersonal = append(automaticPersonal, automatic)
		if e = json.Unmarshal(data, &i.Services); e != nil {
			return nil, e
		}
		candidates = append(candidates, i)
	}
	e = rows.Err()
	if e != nil {
		return nil, e
	}
	if e = rows.Close(); e != nil {
		rowsClosed = true
		return nil, e
	}
	rowsClosed = true
	if len(candidates) > MaxCandidateMachines {
		return nil, ErrDenied
	}
	out := []wire.Item{}
	for index, i := range candidates {
		allowed := []wire.PublishedPort{}
		for _, p := range i.Services {
			var e error
			if !automaticPersonal[index] || p.Port < 1024 {
				_, e = ResolveTx(ctx, tx, actor, i.MachineID, p.Port)
			}
			if e == nil {
				hostname, err := NativeBrowserHostname(i.Alias, p.Port, s.LocalAccessBaseDomain)
				if err != nil {
					return nil, err
				}
				p.BrowserURL = "https://" + hostname
				allowed = append(allowed, p)
			} else if e != ErrDenied {
				return nil, e
			}
		}
		if len(allowed) > 0 {
			i.Services = allowed
			out = append(out, i)
		}
		if len(out) > MaxMachines {
			return nil, ErrDenied
		}
	}
	// Duplicate aliases from separately shared machines cannot identify one
	// local browser target. Preserve service metadata; certificate planning
	// rejects the ambiguous alias instead of publishing a misleading URL.
	counts := map[string]int{}
	for _, item := range out {
		counts[item.Alias]++
	}
	for i := range out {
		if counts[out[i].Alias] > 1 {
			for p := range out[i].Services {
				out[i].Services[p].BrowserURL = ""
			}
		}
	}
	return out, tx.Commit()
}

// database/sql returns sql.ErrNoRows directly from Row.Scan. Do not let a
// joined or independently wrapped storage error turn into an authorization
// denial.
func machineServiceRowMissing(err error) bool { return err == sql.ErrNoRows }

// ReserveBrowserHostname binds a random public name to the exact machine/port.
// Authorization belongs to the caller and must precede this allocation.
func ReserveBrowserHostname(ctx context.Context, tx *sql.Tx, machine string, port int, domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if port < 1 || port > 65535 {
		return "", wire.ErrInvalid
	}
	for attempt := 0; attempt < 8; attempt++ {
		label, err := naming.PublicSlug(nil)
		if err != nil {
			return "", err
		}
		candidate := label + "." + domain
		if !validBrowserHostname(candidate) {
			return "", wire.ErrInvalid
		}
		var host string
		if err = tx.QueryRowContext(ctx, `SELECT paperboat.reserve_machine_hostname($1,$2,$3)`, machine, port, candidate).Scan(&host); err != nil {
			return "", err
		}
		if host != "" {
			return host, nil
		}
	}
	return "", ErrConflict
}

func validBrowserHostname(host string) bool {
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return false
			}
		}
	}
	return true
}

// NativeBrowserHostname is the authoritative readable native browser URL name.
// It is resolved through account-scoped local routing, never public edge state.
func NativeBrowserHostname(alias string, port int, baseDomain string) (string, error) {
	if !machinealias.Valid(alias) || port < 1 || port > 65535 || (!validBrowserHostname(baseDomain) || net.ParseIP(baseDomain) != nil) {
		return "", wire.ErrInvalid
	}
	hostname := strconv.Itoa(port) + "." + alias + "." + baseDomain
	if !validBrowserHostname(hostname) {
		return "", wire.ErrInvalid
	}
	return hostname, nil
}

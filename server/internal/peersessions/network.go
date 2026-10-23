package peersessions

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat-server/internal/admission"
	"github.com/pinksaucepasta/paperboat-server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"golang.org/x/crypto/curve25519"
)

// networkConfigLimit bounds one signed configuration, relay grant or regional candidate token;
// it is the client's bound (shared/peernetwork), so a configuration the server signs is one the
// CLI accepts.
const networkConfigLimit = peernetwork.MaxConfigurationBytes

// maximumAccountEndpoints is the stated product maximum of endpoints in one account, shared
// with the CLI's trusted-key and configuration bounds.
const maximumAccountEndpoints = peernetwork.MaxEndpoints

type NetworkRegistration struct {
	OperationID                                                    string
	UserID, EndpointID, Role, MachineID                            string
	EndpointGeneration, MachineGeneration, ExpectedKeyGeneration   int64
	WireGuardPublicKey, DiscoPublicKey, QUICCertificateFingerprint []byte
}
type NetworkRegistrationResult struct {
	KeyGeneration  int64  `json:"key_generation"`
	VirtualAddress string `json:"virtual_address"`
}
type NetworkConfigRequest struct {
	OperationID, UserID, EndpointID string
	AdmittedScopes                  []AdmittedScope
	AdmittedCertificates            []AdmittedCertificate
}
type NetworkConfigResult struct {
	Configuration string   `json:"configuration"`
	CandidateSet  string   `json:"candidate_set"`
	RelayGrants   []string `json:"relay_grants"`
	// RelayDenied is set when hosted relay grants were withheld at admission (see
	// RelayDeniedQuotaExhausted); BillingURL then says where to add bandwidth.
	RelayDenied string `json:"relay_denied,omitempty"`
	BillingURL  string `json:"billing_url,omitempty"`
}

type NetworkOptions struct{ STUNServers []string }

type NetworkService struct {
	endpointBuilds keyedLocks
	stunServers    []string
	store          *db.DB
	signer         interface {
		SignNetworkConfiguration(map[string]any) (string, error)
		SignRegionalCandidates(map[string]any) (string, error)
		SignRelayGrant(map[string]any) (string, error)
	}
	issuer string
	now    func() time.Time
	// Relay quota gate (relay_quota.go): nil (off) until ConfigureQuota.
	quotaGate  *bandwidthgate.Gate
	billingURL string
}

func NewNetworkService(store *db.DB, signer interface {
	SignNetworkConfiguration(map[string]any) (string, error)
	SignRegionalCandidates(map[string]any) (string, error)
	SignRelayGrant(map[string]any) (string, error)
}, issuer string, options NetworkOptions) (*NetworkService, error) {
	if store == nil || signer == nil || issuer == "" {
		return nil, ErrInvalid
	}
	if err := (config.Network{STUNServers: options.STUNServers}).Validate(); err != nil {
		return nil, err
	}
	stunServers := append([]string(nil), options.STUNServers...)
	return &NetworkService{store: store, signer: signer, issuer: issuer, now: time.Now, stunServers: stunServers}, nil
}

func (s *NetworkService) Register(ctx context.Context, in NetworkRegistration) (NetworkRegistrationResult, error) {
	if ctx == nil || !bounded(in.OperationID, 8, 128) || !bounded(in.UserID, 1, 256) || !bounded(in.EndpointID, 1, 128) || !validWireGuardPublicKey(in.WireGuardPublicKey) || !validWireGuardPublicKey(in.DiscoPublicKey) || len(in.QUICCertificateFingerprint) != 32 || in.ExpectedKeyGeneration < 0 || (in.Role != "cli" && in.Role != "machine") {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	if in.Role == "cli" && (in.MachineID != "" || in.MachineGeneration != 0) || in.Role == "machine" && (in.MachineID != in.EndpointID || in.MachineGeneration < 1) {
		return NetworkRegistrationResult{}, ErrInvalid
	}
	hash := sha256.Sum256([]byte(in.UserID + "\x00" + in.EndpointID + "\x00" + in.Role + "\x00" + fmt.Sprint(in.EndpointGeneration) + "\x00" + fmt.Sprint(in.MachineGeneration) + "\x00" + fmt.Sprint(in.ExpectedKeyGeneration) + "\x00" + base64.RawURLEncoding.EncodeToString(in.WireGuardPublicKey) + "\x00" + base64.RawURLEncoding.EncodeToString(in.DiscoPublicKey) + "\x00" + hex.EncodeToString(in.QUICCertificateFingerprint)))
	tx, err := db.BeginSerializable(ctx, s.store.SQL())
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var certRole, certEndpoint string
	var certGeneration int64
	err = tx.QueryRowContext(ctx, `SELECT c.role,c.endpoint_id,c.generation FROM paperboat.peer_endpoint_certificates c WHERE c.user_id=$1 AND c.fingerprint=$2 AND c.revoked_at IS NULL AND c.issued_at<=$3 AND c.expires_at>$3`, in.UserID, in.QUICCertificateFingerprint, now).Scan(&certRole, &certEndpoint, &certGeneration)
	if err == sql.ErrNoRows || err == nil && (certRole != in.Role || certEndpoint != in.EndpointID || certGeneration != in.EndpointGeneration) {
		return NetworkRegistrationResult{}, ErrDenied
	}
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	if in.Role == "cli" {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c JOIN paperboat.users u ON u.id=c.user_id AND u.status='active' WHERE c.id=$1 AND c.user_id=$2 AND c.state='active' AND c.revoked_at IS NULL)`, in.EndpointID, in.UserID).Scan(&active)
		if err != nil {
			return NetworkRegistrationResult{}, err
		}
		if !active {
			return NetworkRegistrationResult{}, ErrDenied
		}
	} else {
		var generation int64
		err = tx.QueryRowContext(ctx, `SELECT m.installation_generation FROM paperboat.user_machines m JOIN paperboat.users u ON u.id=m.user_id AND u.status='active' JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL WHERE m.id=$1 AND m.user_id=$2 AND m.state IN ('online','offline') AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, in.EndpointID, in.UserID).Scan(&generation)
		if err == sql.ErrNoRows || err == nil && generation != in.MachineGeneration {
			return NetworkRegistrationResult{}, ErrDenied
		}
		if err != nil {
			return NetworkRegistrationResult{}, err
		}
	}
	var oldHash []byte
	var replayGen int64
	err = tx.QueryRowContext(ctx, `SELECT request_hash,key_generation FROM paperboat.peer_network_registration_operations WHERE operation_id=$1`, in.OperationID).Scan(&oldHash, &replayGen)
	if err == nil {
		if !equalBytes(oldHash, hash[:]) {
			return NetworkRegistrationResult{}, ErrConflict
		}
		var address string
		if err = tx.QueryRowContext(ctx, `SELECT host(virtual_address) FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND revoked_at IS NULL`, in.UserID, in.EndpointID).Scan(&address); err != nil {
			return NetworkRegistrationResult{}, unavailableWithCause(err)
		}
		return NetworkRegistrationResult{replayGen, address}, nil
	}
	if err != sql.ErrNoRows {
		return NetworkRegistrationResult{}, err
	}
	var currentGen int64
	var address string
	var currentWireGuard, currentDisco, currentCertificate []byte
	created := false
	err = tx.QueryRowContext(ctx, `SELECT key_generation,host(virtual_address),wireguard_public_key,disco_public_key,quic_certificate_fingerprint FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2 AND role=$3 AND revoked_at IS NULL FOR UPDATE`, in.UserID, in.EndpointID, in.Role).Scan(&currentGen, &address, &currentWireGuard, &currentDisco, &currentCertificate)
	if err == sql.ErrNoRows {
		if in.ExpectedKeyGeneration != 0 {
			return NetworkRegistrationResult{}, ErrConflict
		}
		created = true
		err = tx.QueryRowContext(ctx, `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,disco_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at) VALUES($1,$2,$3,nullif($4,''),$5,$6,1,$7,$8,$9,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')), $10,$10) RETURNING key_generation,host(virtual_address)`, in.UserID, in.EndpointID, in.Role, in.MachineID, in.EndpointGeneration, in.MachineGeneration, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now).Scan(&currentGen, &address)
	} else if err == nil {
		if currentGen != in.ExpectedKeyGeneration {
			return NetworkRegistrationResult{}, ErrConflict
		}
		if !equalBytes(currentWireGuard, in.WireGuardPublicKey) || !equalBytes(currentDisco, in.DiscoPublicKey) {
			currentGen++
		}
		_, err = tx.ExecContext(ctx, `UPDATE paperboat.peer_network_identities SET endpoint_generation=$1,machine_generation=$2,key_generation=$3,wireguard_public_key=$4,disco_public_key=$5,quic_certificate_fingerprint=$6,revoked_at=NULL,updated_at=$7 WHERE user_id=$8 AND endpoint_id=$9`, in.EndpointGeneration, in.MachineGeneration, currentGen, in.WireGuardPublicKey, in.DiscoPublicKey, in.QUICCertificateFingerprint, now, in.UserID, in.EndpointID)
	}
	if err != nil {
		return NetworkRegistrationResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.peer_network_registration_operations(operation_id,user_id,endpoint_id,request_hash,key_generation,created_at) VALUES($1,$2,$3,$4,$5,$6)`, in.OperationID, in.UserID, in.EndpointID, hash[:], currentGen, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM paperboat.peer_network_registration_operations o WHERE o.user_id=$1 AND o.endpoint_id=$2 AND o.operation_id IN (SELECT operation_id FROM paperboat.peer_network_registration_operations WHERE user_id=$1 AND endpoint_id=$2 ORDER BY key_generation DESC,created_at DESC,operation_id DESC OFFSET 64)`, in.UserID, in.EndpointID); err != nil {
		return NetworkRegistrationResult{}, err
	}
	eventType := "peer_network.key_rotated"
	if created {
		eventType = "peer_network.key_registered"
	} else if equalBytes(currentWireGuard, in.WireGuardPublicKey) && equalBytes(currentDisco, in.DiscoPublicKey) && !equalBytes(currentCertificate, in.QUICCertificateFingerprint) {
		eventType = "peer_network.certificate_rebound"
	}
	auditID := "aud_peer_network_" + hex.EncodeToString(hash[:16])
	metadata, _ := json.Marshal(map[string]any{"endpoint_id": in.EndpointID, "role": in.Role, "key_generation": currentGen, "virtual_address": address})
	if _, err = tx.ExecContext(ctx, `INSERT INTO paperboat.audit_events(id,actor_user_id,actor_type,event_type,resource_type,resource_id,idempotency_key,metadata,created_at) VALUES($1,$2,'user',$3,'peer_network_identity',$4,$5,$6::jsonb,$7) ON CONFLICT (event_type,resource_type,resource_id,idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`, auditID, in.UserID, eventType, in.EndpointID, in.OperationID, metadata, now); err != nil {
		return NetworkRegistrationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return NetworkRegistrationResult{}, err
	}
	return NetworkRegistrationResult{currentGen, address}, nil
}

type networkBinding struct {
	AccountID                       string   `json:"account_id"`
	EndpointID                      string   `json:"endpoint_id"`
	Role                            string   `json:"role"`
	MachineID                       string   `json:"machine_id"`
	EndpointGeneration              int64    `json:"endpoint_generation"`
	MachineGeneration               int64    `json:"machine_generation"`
	KeyGeneration                   int64    `json:"key_generation"`
	WireGuardPublicKey              string   `json:"wireguard_public_key"`
	DiscoPublicKey                  string   `json:"disco_public_key"`
	QUICCertificateFingerprint      string   `json:"quic_certificate_fingerprint"`
	QUICPublicKey                   string   `json:"quic_public_key"`
	AdmittedCertificateFingerprints []string `json:"admitted_certificate_fingerprints,omitempty"`
	VirtualAddress                  string   `json:"virtual_address"`
}
type networkScope struct {
	ResourceKind       string `json:"resource_kind"`
	ResourceID         string `json:"resource_id"`
	ResourceGeneration int64  `json:"resource_generation"`
	Capability         string `json:"capability"`
	Direction          string `json:"direction"`
	Port               int    `json:"port"`
	ExpiresAt          int64  `json:"expires_at"`
}
type networkPeer struct {
	Identity networkBinding `json:"identity"`
	Scopes   []networkScope `json:"scopes"`
}

func (s *NetworkService) Configuration(ctx context.Context, in NetworkConfigRequest) (NetworkConfigResult, error) {
	// Match the database owner's bounded serializable retry policy. A failed
	// attempt is fully rolled back, including its configuration generation.
	// Wake bursts send several configuration requests for one endpoint at once;
	// built concurrently they all bump the same generation row and abort each
	// other past the retry budget. One build per endpoint at a time instead.
	unlock := s.endpointBuilds.lock(in.EndpointID)
	defer unlock()
	for attempt := 1; ; attempt++ {
		result, err := s.configuration(ctx, in)
		var pg *pgconn.PgError
		if err == nil || attempt >= 10 || !errors.As(err, &pg) || pg.Code != "40001" && pg.Code != "40P01" {
			return result, err
		}
		backoff := min(5*time.Millisecond<<min(attempt-1, 5), 200*time.Millisecond)
		half := backoff / 2
		timer := time.NewTimer(half + time.Duration(time.Now().UnixNano()%int64(half+1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return NetworkConfigResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *NetworkService) configuration(ctx context.Context, in NetworkConfigRequest) (NetworkConfigResult, error) {
	if ctx == nil || !bounded(in.OperationID, 8, 128) || !bounded(in.UserID, 1, 256) || !bounded(in.EndpointID, 1, 128) {
		return NetworkConfigResult{}, ErrInvalid
	}
	if err := validateAdmittedCertificates(in.AdmittedCertificates, len(in.AdmittedScopes) > 0); err != nil {
		return NetworkConfigResult{}, err
	}
	live, err := admittedScopeIDs(in.AdmittedScopes)
	if err != nil {
		return NetworkConfigResult{}, err
	}

	now := s.now().UTC()
	tx, err := db.BeginSerializable(ctx, s.store.SQL())
	if err != nil {
		return NetworkConfigResult{}, err
	}
	defer tx.Rollback()
	self, certExpiry, err := loadNetworkBinding(ctx, tx, in.UserID, in.EndpointID, now, len(in.AdmittedScopes) > 0)
	if err == sql.ErrNoRows || err == ErrUnavailable {
		return NetworkConfigResult{}, ErrDenied
	}
	if err != nil {
		return NetworkConfigResult{}, err
	}
	var generation int64
	err = tx.QueryRowContext(ctx, `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+1,updated_at=$1 WHERE user_id=$2 AND endpoint_id=$3 RETURNING config_generation`, now, in.UserID, in.EndpointID).Scan(&generation)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.cli_client_session_id,a.user_machine_id,a.user_id,m.user_id,a.expires_at,array_to_json(a.capabilities),coalesce(a.terminal_session_id,''),coalesce(a.terminal_role,'')
		FROM paperboat.user_machine_access_sessions a
		JOIN paperboat.cli_client_sessions c ON c.id=a.cli_client_session_id AND c.user_id=a.user_id AND c.state='active' AND c.revoked_at IS NULL
		JOIN paperboat.user_machines m ON m.id=a.user_machine_id AND m.environment_id=a.environment_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL
		JOIN paperboat.control_environments e ON e.id=a.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL
		JOIN paperboat.users actor ON actor.id=a.user_id AND actor.status='active'
		JOIN paperboat.users owner ON owner.id=m.user_id AND owner.status='active'
		WHERE a.state='active' AND a.revoked_at IS NULL
 AND (a.expires_at>$3 OR a.id=ANY($4::text[]))
		AND ((a.cli_client_session_id=$2 AND a.user_id=$1) OR (a.user_machine_id=$2 AND m.user_id=$1))
		AND ((a.machine_service_port IS NOT NULL AND paperboat.machine_service_session_allowed(a.id))
		 OR (a.terminal_role IN ('viewer','interactive') AND a.team_id IS NOT NULL AND m.state='online' AND m.online AND m.configured_capabilities @> ARRAY['terminal_host'] AND m.observed_capabilities @> ARRAY['terminal_host']
		 AND paperboat.terminal_session_role(a.user_id,a.terminal_session_id)=a.terminal_role
		 AND EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members tm ON tm.team_id=t.team_id AND tm.account_id=a.user_id JOIN paperboat.team_resource_bindings b ON b.team_id=t.team_id AND b.resource_kind='terminal_session' AND b.resource_id=a.terminal_session_id AND b.active LEFT JOIN paperboat.team_terminal_session_grants sg ON sg.team_id=t.team_id AND sg.terminal_session_id=a.terminal_session_id AND sg.audience='selected_member' AND sg.account_id=a.user_id LEFT JOIN paperboat.team_terminal_session_grants ag ON ag.team_id=t.team_id AND ag.terminal_session_id=a.terminal_session_id AND ag.audience='all_members' WHERE t.team_id=a.team_id AND t.deleted_at IS NULL AND paperboat.team_paid_entitled(t.team_id) AND tm.active AND tm.membership_generation=a.terminal_membership_generation AND b.generation=a.terminal_binding_generation AND CASE WHEN sg.account_id IS NOT NULL THEN sg.generation ELSE ag.generation END=a.terminal_grant_generation))
		 OR (a.machine_service_port IS NULL AND coalesce(a.terminal_role,'owner')='owner' AND a.team_id IS NULL AND a.user_id=m.user_id AND m.owner_team_id IS NULL)
		 OR (coalesce(a.terminal_role,'owner')='owner' AND a.team_id IS NOT NULL AND NOT ('private_access'=ANY(a.capabilities)) AND NOT EXISTS(
		  SELECT 1 FROM unnest(a.capabilities) capability
		  WHERE NOT paperboat.team_machine_capability_allowed(a.user_id,a.team_id,a.user_machine_id,capability))))
		ORDER BY (a.expires_at>$3) DESC, a.expires_at DESC, a.id LIMIT 129`, in.UserID, in.EndpointID, now, live["machine_access"])
	if err != nil {
		return NetworkConfigResult{}, err
	}
	type grantRow struct {
		id, cli, machine, cliAccount, machineAccount, kind, terminalSession, terminalRole string
		expiry                                                                            time.Time
		capabilities                                                                      []string
	}
	grants := make([]grantRow, 0, 128)
	for rows.Next() {
		var g grantRow
		var rawCapabilities []byte
		if err = rows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry, &rawCapabilities, &g.terminalSession, &g.terminalRole); err != nil {
			rows.Close()
			return NetworkConfigResult{}, unavailableWithCause(err)
		}
		var storedCapabilities []string
		if err = json.Unmarshal(rawCapabilities, &storedCapabilities); err != nil {
			rows.Close()
			return NetworkConfigResult{}, unavailableWithCause(err)
		}
		g.kind = "machine_access"
		if g.terminalRole == "viewer" || g.terminalRole == "interactive" {
			g.capabilities = []string{"terminal"}
			grants = append(grants, g)
			continue
		}
		seenCapabilities := make(map[string]bool, len(storedCapabilities))
		for _, capability := range storedCapabilities {
			projected := capability
			switch capability {
			case "terminal", "exec", "managed_ssh", "private_access":
			case "files":
				projected = "file_transfer"
			case "preview_manage", "tunnel_manage":
				continue
			default:
				rows.Close()
				slog.Warn("peer network authority unavailable", "site", "network.go:326")
				return NetworkConfigResult{}, ErrUnavailable
			}
			if !seenCapabilities[projected] {
				seenCapabilities[projected] = true
				g.capabilities = append(g.capabilities, projected)
			}
		}
		sort.Strings(g.capabilities)
		grants = append(grants, g)
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return NetworkConfigResult{}, rowErr
	}
	grants = boundGrants(grants)
	inspectorRows, err := tx.QueryContext(ctx, `SELECT credential_id,native_cli_session_id,native_machine_id,account_id,owner_account_id,expires_at FROM paperboat.inspector_native_admissions WHERE ((native_cli_session_id=$1 AND account_id=$2) OR (native_machine_id=$1 AND owner_account_id=$2)) AND (expires_at>$3 OR credential_id=ANY($4::text[])) ORDER BY credential_id LIMIT 129`, in.EndpointID, in.UserID, now, live["inspector"])
	if err != nil {
		return NetworkConfigResult{}, err
	}
	for inspectorRows.Next() {
		var g grantRow
		if err = inspectorRows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry); err != nil {
			inspectorRows.Close()
			return NetworkConfigResult{}, err
		}
		g.kind = "inspector"
		g.capabilities = []string{"inspector"}
		grants = append(grants, g)
	}
	err = inspectorRows.Err()
	inspectorRows.Close()
	if err != nil {
		return NetworkConfigResult{}, err
	}
	grants = boundGrants(grants)

	compareRows, err := tx.QueryContext(ctx, `SELECT DISTINCT ON (a.assignment_id,a.native_cli_session_id,a.machine_id) a.assignment_id,a.native_cli_session_id,a.machine_id,a.account_id,a.account_id,a.expires_at
 FROM config_compare_admissions a
 JOIN cli_client_sessions c ON c.id=a.native_cli_session_id AND c.user_id=a.account_id AND c.state='active' AND c.revoked_at IS NULL
 JOIN users u ON u.id=a.account_id AND u.status='active'
 JOIN user_machines m ON m.id=a.machine_id AND m.user_id=a.account_id AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.installation_generation=a.installation_generation
 JOIN control_config_assignments assignment ON assignment.id=a.assignment_id AND assignment.version=a.assignment_version AND assignment.machine_id=a.machine_id AND assignment.consent_state='accepted'
 JOIN control_config_sync_statuses status ON status.environment_id=a.environment_id AND status.assignment_id=a.assignment_id AND status.machine_id=a.machine_id AND status.installation_generation=a.installation_generation AND status.remote_revision=a.expected_remote_revision
 WHERE ((a.native_cli_session_id=$2 AND a.account_id=$1) OR (a.machine_id=$2 AND a.account_id=$1))
 AND (a.expires_at>$3 OR a.assignment_id=ANY($4::text[]))
 AND ((a.workspace_id='personal' AND assignment.adopted_team_id IS NULL) OR (assignment.adopted_team_id=a.workspace_id AND EXISTS(SELECT 1 FROM teams t JOIN team_members member USING(team_id) WHERE t.team_id=a.workspace_id AND t.deleted_at IS NULL AND NOT t.suspended AND member.account_id=a.account_id AND member.active)))
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(status.conflicts) conflict WHERE conflict->>'path'=a.config_path AND conflict->>'revision'=a.conflict_revision) ORDER BY a.assignment_id,a.native_cli_session_id,a.machine_id,a.expires_at DESC LIMIT 129`, in.UserID, in.EndpointID, now, live["config_compare"])
	if err != nil {
		return NetworkConfigResult{}, err
	}
	for compareRows.Next() {
		var g grantRow
		if err = compareRows.Scan(&g.id, &g.cli, &g.machine, &g.cliAccount, &g.machineAccount, &g.expiry); err != nil {
			compareRows.Close()
			return NetworkConfigResult{}, err
		}
		g.kind = "config_compare"
		g.capabilities = []string{"config_compare"}
		grants = append(grants, g)
	}
	err = compareRows.Err()
	compareRows.Close()
	if err != nil {
		return NetworkConfigResult{}, err
	}
	grants = boundGrants(grants)
	byPeer := map[string]*networkPeer{}
	admittedPeers := map[string]bool{}
	scopes := 0
	expiry := admission.Expiry(s.signer, now)
	if certExpiry.After(now) && certExpiry.Before(expiry) {
		expiry = certExpiry
	}
	for _, g := range grants {
		// Preserve expired admission scopes until explicit withdrawal. Machines
		// use their original expiry to reject new operations, not to kill work
		// already admitted under this exact access-session identity.
		if len(g.capabilities) == 0 {
			continue
		}
		peerID, peerAccount, direction := g.machine, g.machineAccount, "dial"
		if in.EndpointID == g.machine {
			peerID, peerAccount, direction = g.cli, g.cliAccount, "accept"
		}
		admitted := slices.Contains(live[g.kind], g.id)
		if admitted {
			admittedPeers[peerID] = true
		}
		p := byPeer[peerID]
		if p == nil {
			binding, peerCertExpiry, e := loadNetworkBinding(ctx, tx, peerAccount, peerID, now, admitted)
			if e == sql.ErrNoRows || e == ErrUnavailable {
				continue
			}
			if e != nil {
				return NetworkConfigResult{}, e
			}
			if len(byPeer) >= maximumNetworkAccessPeers {
				continue
			}
			if peerCertExpiry.After(now) && peerCertExpiry.Before(expiry) {
				expiry = peerCertExpiry
			}
			p = &networkPeer{Identity: binding}
			byPeer[peerID] = p
		}
		for _, capability := range g.capabilities {
			if scopes >= maximumNetworkAccessScopes {
				break
			}
			p.Scopes = append(p.Scopes, networkScope{g.kind, g.id, 1, capability, direction, 443, g.expiry.Unix()})
			scopes++
		}
	}
	// Durable personal-machine connectivity is distinct from application access.
	// Exact access-session scopes above still authorize every application stream.
	mesh, err := personalNetworkPeers(ctx, tx, self, now, &expiry, admittedPeers, live["machine_network"])
	if err != nil {
		return NetworkConfigResult{}, err
	}
	for _, binding := range mesh {
		p := byPeer[binding.EndpointID]
		if p == nil {
			if len(byPeer) >= maximumAccountEndpoints {
				return NetworkConfigResult{}, ErrResourceLimit
			}
			p = &networkPeer{Identity: binding}
			byPeer[binding.EndpointID] = p
		}
		if scopes >= peernetwork.MaxScopes {
			return NetworkConfigResult{}, ErrResourceLimit
		}
		p.Scopes = append(p.Scopes, personalNetworkScope(self, binding, expiry))
		scopes++
	}
	if err := retainAdmittedCertificates(ctx, tx, &self, byPeer, in.AdmittedCertificates, now); err != nil {
		return NetworkConfigResult{}, err
	}
	peerIDs := make([]string, 0, len(byPeer))
	for id := range byPeer {
		peerIDs = append(peerIDs, id)
	}
	sort.Strings(peerIDs)
	peers := make([]networkPeer, 0, len(peerIDs))
	for _, id := range peerIDs {
		p := byPeer[id]
		sort.Slice(p.Scopes, func(i, j int) bool {
			if p.Scopes[i].ResourceID != p.Scopes[j].ResourceID {
				return p.Scopes[i].ResourceID < p.Scopes[j].ResourceID
			}
			return p.Scopes[i].Capability < p.Scopes[j].Capability
		})
		peers = append(peers, *p)
	}
	claims := map[string]any{"version": 1, "iss": s.issuer, "aud": "paperboat-network", "iat": now.Unix(), "exp": expiry.Unix(), "generation": generation, "self": bindingMap(self), "peers": peers}
	token, err := s.signer.SignNetworkConfiguration(claims)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if len(token) > networkConfigLimit {
		return NetworkConfigResult{}, ErrResourceLimit
	}
	candidates, err := s.regionalCandidates(ctx, tx, in.UserID, in.EndpointID, generation, now, expiry)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if len(candidates) > networkConfigLimit {
		slog.Warn("peer network authority unavailable", "site", "network.go:500")
		return NetworkConfigResult{}, ErrUnavailable
	}
	quotaDenied := s.relayQuotaWithheld(ctx, self.AccountID, now)
	relayGrants, withheld, err := s.relayGrants(ctx, tx, self, peers, generation, now, expiry, quotaDenied)
	if err != nil {
		return NetworkConfigResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return NetworkConfigResult{}, err
	}
	result := NetworkConfigResult{Configuration: token, CandidateSet: candidates, RelayGrants: relayGrants}
	if withheld {
		result.RelayDenied, result.BillingURL = RelayDeniedQuotaExhausted, s.billingURL
	}
	return result, nil
}

func personalNetworkScopeID(self, peer networkBinding) string {
	first, second := self.EndpointID, peer.EndpointID
	if second < first {
		first, second = second, first
	}
	hash := sha256.Sum256([]byte(self.AccountID + "\x00" + first + "\x00" + second))
	return "network_" + hex.EncodeToString(hash[:])
}

func personalNetworkScope(self, peer networkBinding, expiry time.Time) networkScope {
	direction := "dial"
	if self.Role == "machine" && (peer.Role == "cli" || self.EndpointID > peer.EndpointID) {
		direction = "accept"
	}
	return networkScope{"machine_network", personalNetworkScopeID(self, peer), 1, "connect", direction, 443, expiry.Unix()}
}

func personalNetworkPeers(ctx context.Context, tx *sql.Tx, self networkBinding, now time.Time, expiry *time.Time, admittedPeers map[string]bool, admittedNetworks []string) ([]networkBinding, error) {
	if self.Role == "machine" {
		var personal bool
		if err := tx.QueryRowContext(ctx, `SELECT owner_team_id IS NULL FROM paperboat.user_machines WHERE id=$1 AND user_id=$2`, self.EndpointID, self.AccountID).Scan(&personal); err != nil {
			return nil, err
		}
		if !personal {
			return nil, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT n.endpoint_id FROM paperboat.peer_network_identities n
 WHERE n.user_id=$1 AND n.endpoint_id<>$2 AND n.revoked_at IS NULL AND
 ((n.role='machine' AND EXISTS(SELECT 1 FROM paperboat.user_machines m WHERE m.id=n.machine_id AND m.user_id=n.user_id AND m.owner_team_id IS NULL AND m.revoked_at IS NULL AND m.deleted_at IS NULL))
 OR ($3='machine' AND n.role='cli' AND EXISTS(SELECT 1 FROM paperboat.cli_client_sessions c WHERE c.id=n.endpoint_id AND c.user_id=n.user_id AND c.state='active' AND c.revoked_at IS NULL)))
 ORDER BY n.endpoint_id LIMIT $4`, self.AccountID, self.EndpointID, self.Role, maximumAccountEndpoints+1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Every endpoint of the account is a peer; the bound is the stated product
	// maximum, and the signed configuration size is checked where it is built.
	if len(ids) > maximumAccountEndpoints {
		return nil, ErrResourceLimit
	}
	peers := make([]networkBinding, 0, len(ids))
	for _, id := range ids {
		binding, certExpiry, err := loadNetworkBinding(ctx, tx, self.AccountID, id, now, admittedPeers[id] || slices.Contains(admittedNetworks, personalNetworkScopeID(self, networkBinding{EndpointID: id})))
		if err == sql.ErrNoRows || err == ErrUnavailable {
			continue
		}
		if err != nil {
			return nil, err
		}
		if certExpiry.After(now) && certExpiry.Before(*expiry) {
			*expiry = certExpiry
		}
		peers = append(peers, binding)
	}
	return peers, nil
}

// relayGrants signs one grant per eligible relay node. With quotaDenied only customer-run
// relays (never metered against Paperboat's allowance) are granted; withheld reports that a
// hosted grant was refused for that reason.
func (s *NetworkService) relayGrants(ctx context.Context, tx *sql.Tx, self networkBinding, peers []networkPeer, generation int64, now, authorityExpiry time.Time, quotaDenied bool) ([]string, bool, error) {
	controlPeers, err := relayControlPeers(self, peers)
	if err != nil {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,node_generation,process_epoch,registry_expires_at,roles @> ARRAY['peer_relay']::text[],peer_relay_wireguard_public_key,peer_relay_disco_public_key,host(peer_relay_virtual_address),EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=control_tunnel_nodes.id AND i.scope_kind IN ('account','team')) FROM paperboat.control_tunnel_nodes WHERE region IS NOT NULL AND paperboat.node_pool_eligible(id,$1,'relay') AND state='ready' AND ready AND last_heartbeat_at>$2::timestamptz-interval '15 seconds' AND registry_expires_at>$2::timestamptz AND capacity_observed_at>$2::timestamptz-interval '15 seconds' AND capacity_used<capacity_limit-capacity_limit/10 AND drain_deadline IS NULL AND roles @> ARRAY['relay']::text[] AND transports @> ARRAY['derp_quic']::text[] ORDER BY region,id LIMIT 33`, self.AccountID, now)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	chunks := relayGrantChunks(peers)
	grants := make([]string, 0, len(chunks))
	withheld := false
	nodes := 0
	for rows.Next() {
		if nodes == 32 {
			return nil, false, ErrResourceLimit
		}
		nodes++
		var nodeID, processEpoch string
		var nodeGeneration int64
		var nodeExpiry time.Time
		var peerRelayEnabled bool
		var peerRelayWG, peerRelayDisco []byte
		var peerRelayAddress sql.NullString
		var customerOwned bool
		if err := rows.Scan(&nodeID, &nodeGeneration, &processEpoch, &nodeExpiry, &peerRelayEnabled, &peerRelayWG, &peerRelayDisco, &peerRelayAddress, &customerOwned); err != nil {
			return nil, false, err
		}
		if quotaDenied && !customerOwned {
			withheld = true
			continue
		}
		if peerRelayEnabled && (len(peerRelayWG) != 32 || len(peerRelayDisco) != 32 || !peerRelayAddress.Valid) {
			slog.Warn("peer network authority unavailable", "site", "network.go:620")
			return nil, false, ErrUnavailable
		}
		expires := now.Add(time.Minute)
		if authorityExpiry.Before(expires) {
			expires = authorityExpiry
		}
		if nodeExpiry.Before(expires) {
			expires = nodeExpiry
		}
		// One grant per chunk of at most maximumRelayGrantPeers peers, all with
		// identical identity, iat, exp and generation; the relay and the CLI merge
		// them. Relay control and the peer relay ride in the first chunk only.
		for i, chunk := range chunks {
			first := i == 0
			claims := map[string]any{"version": 1, "iss": s.issuer, "aud": "paperboat-relay", "iat": now.Unix(), "exp": expires.Unix(), "generation": generation, "account_id": self.AccountID, "endpoint_id": self.EndpointID, "wireguard_public_key": self.WireGuardPublicKey, "quic_certificate_fingerprint": self.QUICCertificateFingerprint, "quic_public_key": self.QUICPublicKey, "node_id": nodeID, "node_generation": nodeGeneration, "process_epoch": processEpoch, "peers": chunk}
			if len(self.AdmittedCertificateFingerprints) > 0 {
				claims["admitted_certificate_fingerprints"] = self.AdmittedCertificateFingerprints
			}
			if self.DiscoPublicKey != "" {
				claims["disco_public_key"] = self.DiscoPublicKey
			}
			peerRelayReady := peerRelayEnabled && self.DiscoPublicKey != "" && peersHaveDiscoKeys(peers)
			if first && peerRelayReady && len(controlPeers) != 0 {
				claims["relay_control_peers"] = controlPeers
			}
			if first && peerRelayReady {
				claims["peer_relay"] = map[string]any{
					"wireguard_public_key": base64.RawURLEncoding.EncodeToString(peerRelayWG),
					"disco_public_key":     base64.RawURLEncoding.EncodeToString(peerRelayDisco),
					"virtual_address":      peerRelayAddress.String,
				}
			}
			grant, err := s.signer.SignRelayGrant(claims)
			if err != nil {
				return nil, false, err
			}
			if len(grant) > networkConfigLimit {
				slog.Warn("peer network authority unavailable", "site", "network.go:653")
				return nil, false, ErrUnavailable
			}
			grants = append(grants, grant)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return grants, withheld, nil
}

func relayControlPeers(self networkBinding, peers []networkPeer) ([]string, error) {
	result := make([]string, 0, len(peers))
	for _, peer := range peers {
		if peer.Identity.AccountID != self.AccountID || peer.Identity.EndpointID == self.EndpointID {
			continue
		}
		if peer.Identity.WireGuardPublicKey == "" {
			slog.Warn("peer network authority unavailable", "site", "network.go:670")
			return nil, ErrUnavailable
		}
		result = append(result, peer.Identity.WireGuardPublicKey)
	}
	sort.Strings(result)
	if len(result) > maximumRelayControlPeers {
		// A relay grant names a bounded set of the account's peers. Beyond it the extra peers
		// simply have no relay fallback (they still connect directly); the refresh must not fail.
		slog.Warn("relay grant names a bounded subset of the account's peers", "kept", maximumRelayControlPeers, "peers", len(result))
		result = result[:maximumRelayControlPeers]
	}
	return result, nil
}

// maximumRelayGrantPeers is the relay's bound on one grant (relay derpquic
// authority and mint both refuse more); more peers are carried in more grants.
const maximumRelayGrantPeers = 64

func relayGrantChunks(peers []networkPeer) [][]map[string]any {
	ordered := slices.Clone(peers)
	slices.SortStableFunc(ordered, func(a, b networkPeer) int {
		return strings.Compare(a.Identity.WireGuardPublicKey, b.Identity.WireGuardPublicKey)
	})
	chunks := [][]map[string]any{{}}
	for _, peer := range ordered {
		if len(chunks[len(chunks)-1]) == maximumRelayGrantPeers {
			chunks = append(chunks, nil)
		}
		entry := map[string]any{"wireguard_public_key": peer.Identity.WireGuardPublicKey, "scopes": peer.Scopes}
		if peer.Identity.DiscoPublicKey != "" {
			entry["disco_public_key"] = peer.Identity.DiscoPublicKey
		}
		chunks[len(chunks)-1] = append(chunks[len(chunks)-1], entry)
	}
	return chunks
}

func peersHaveDiscoKeys(peers []networkPeer) bool {
	for _, peer := range peers {
		if peer.Identity.DiscoPublicKey == "" {
			return false
		}
	}
	return true
}

type regionalNode struct {
	NodeID, ProcessEpoch, Region, FailureDomain string
	NodeGeneration                              int64
	Roles, Transports                           []string
	EndpointHost                                string
	EndpointTCPPort, EndpointQUICPort           int
	State                                       string
	ObservedAt, ExpiresAt, CapacityObservedAt   int64
	CapacityLimit, CapacityUsed                 int64
	DrainDeadline                               *int64 `json:",omitempty"`
}

func (s *NetworkService) regionalCandidates(ctx context.Context, tx *sql.Tx, accountID, endpointID string, generation int64, now, authorityExpiry time.Time) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,node_generation,process_epoch,region,failure_domain,array_to_json(roles),array_to_json(transports),coalesce(endpoint_host,''),coalesce(endpoint_tcp_port,0),coalesce(endpoint_quic_port,0),state,last_heartbeat_at,registry_expires_at,capacity_limit,capacity_used,capacity_observed_at,drain_deadline,tls_spki_sha256 FROM paperboat.control_tunnel_nodes WHERE region IS NOT NULL AND paperboat.node_pool_eligible(id,$1,'relay') AND state='ready' AND ready AND last_heartbeat_at>$2::timestamptz-interval '15 seconds' AND registry_expires_at>$2::timestamptz AND capacity_observed_at>$2::timestamptz-interval '15 seconds' AND capacity_used<capacity_limit-capacity_limit/10 AND drain_deadline IS NULL ORDER BY region,id LIMIT 33`, accountID, now)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	nodes := make([]map[string]any, 0, 32)
	for rows.Next() {
		var n regionalNode
		var roles, transports []byte
		var observed, expires, capacityAt time.Time
		var drain sql.NullTime
		var tlsPin []byte
		if err := rows.Scan(&n.NodeID, &n.NodeGeneration, &n.ProcessEpoch, &n.Region, &n.FailureDomain, &roles, &transports, &n.EndpointHost, &n.EndpointTCPPort, &n.EndpointQUICPort, &n.State, &observed, &expires, &n.CapacityLimit, &n.CapacityUsed, &capacityAt, &drain, &tlsPin); err != nil {
			return "", err
		}
		if err := json.Unmarshal(roles, &n.Roles); err != nil {
			return "", unavailableWithCause(err)
		}
		if err := json.Unmarshal(transports, &n.Transports); err != nil {
			return "", unavailableWithCause(err)
		}
		if len(nodes) == 32 {
			return "", ErrResourceLimit
		}
		n.ObservedAt, n.ExpiresAt, n.CapacityObservedAt = observed.Unix(), expires.Unix(), capacityAt.Unix()
		m := map[string]any{"node_id": n.NodeID, "node_generation": n.NodeGeneration, "process_epoch": n.ProcessEpoch, "region": n.Region, "failure_domain": n.FailureDomain, "roles": n.Roles, "transports": n.Transports, "endpoint_host": n.EndpointHost, "endpoint_tcp_port": n.EndpointTCPPort, "endpoint_quic_port": n.EndpointQUICPort, "state": n.State, "observed_at": n.ObservedAt, "expires_at": n.ExpiresAt, "capacity_limit": n.CapacityLimit, "capacity_used": n.CapacityUsed, "capacity_observed_at": n.CapacityObservedAt}
		if len(tlsPin) > 0 {
			if len(tlsPin) != sha256.Size {
				slog.Warn("peer network authority unavailable", "site", "network.go:743")
				return "", ErrUnavailable
			}
			m["tls_spki_sha256"] = base64.RawURLEncoding.EncodeToString(tlsPin)
		}
		if drain.Valid {
			m["drain_deadline"] = drain.Time.Unix()
		}
		nodes = append(nodes, m)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	expires := now.Add(time.Minute)
	if authorityExpiry.Before(expires) {
		expires = authorityExpiry
	}
	return s.signRegionalCandidates(map[string]any{"schema": "paperboat.regional-candidates.v1", "iss": s.issuer, "aud": "paperboat-regional-candidates", "account_id": accountID, "endpoint_id": endpointID, "authorization_generation": generation, "generation": generation, "iat": now.Unix(), "exp": expires.Unix(), "nodes": nodes})
}

func (s *NetworkService) signRegionalCandidates(claims map[string]any) (string, error) {
	if len(s.stunServers) != 0 {
		claims["stun_servers"] = append([]string(nil), s.stunServers...)
	}
	return s.signer.SignRegionalCandidates(claims)
}

func loadNetworkBinding(ctx context.Context, tx *sql.Tx, userID, endpointID string, now time.Time, admitted bool) (networkBinding, time.Time, error) {
	var b networkBinding
	var key, disco, fingerprint, quicPublic []byte
	var expiry time.Time
	err := tx.QueryRowContext(ctx, `SELECT n.user_id,n.endpoint_id,n.role,coalesce(n.machine_id,''),n.endpoint_generation,n.machine_generation,n.key_generation,n.wireguard_public_key,n.disco_public_key,n.quic_certificate_fingerprint,c.quic_public_key,host(n.virtual_address),c.expires_at FROM paperboat.peer_network_identities n JOIN paperboat.users u ON u.id=n.user_id AND u.status='active' JOIN paperboat.peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint AND c.user_id=n.user_id AND c.endpoint_id=n.endpoint_id AND c.generation=n.endpoint_generation AND c.role=n.role
AND (c.revoked_at IS NULL OR ($4 AND c.revocation_reason='certificate_superseded'
 AND EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificates replacement
 WHERE replacement.user_id=c.user_id AND replacement.endpoint_id=c.endpoint_id
 AND replacement.role=c.role AND replacement.generation=c.generation
 AND replacement.quic_public_key=c.quic_public_key AND replacement.signer_public_key=c.signer_public_key
 AND replacement.revoked_at IS NULL AND replacement.issued_at<=$3 AND replacement.serial>c.serial)))
AND NOT EXISTS(SELECT 1 FROM paperboat.peer_endpoint_certificate_revocations r WHERE r.certificate_fingerprint=c.fingerprint)
AND c.issued_at<=$3 AND (c.expires_at>$3 OR $4) WHERE n.user_id=$1 AND n.endpoint_id=$2 AND n.revoked_at IS NULL`, userID, endpointID, now, admitted).Scan(&b.AccountID, &b.EndpointID, &b.Role, &b.MachineID, &b.EndpointGeneration, &b.MachineGeneration, &b.KeyGeneration, &key, &disco, &fingerprint, &quicPublic, &b.VirtualAddress, &expiry)
	if err != nil {
		return b, time.Time{}, err
	}
	if !admitted && expiry.Unix() <= now.Unix() {
		slog.Warn("peer network authority unavailable", "site", "network.go:786")
		return b, time.Time{}, ErrUnavailable
	}
	if b.Role == "cli" {
		var active bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.cli_client_sessions WHERE id=$1 AND user_id=$2 AND state='active' AND revoked_at IS NULL)`, endpointID, userID).Scan(&active)
		if err != nil {
			return b, time.Time{}, err
		}
		if !active {
			slog.Warn("peer network authority unavailable", "site", "network.go:795")
			return b, time.Time{}, ErrUnavailable
		}
	} else {
		var gen int64
		err = tx.QueryRowContext(ctx, `SELECT m.installation_generation FROM paperboat.user_machines m JOIN paperboat.control_environments e ON e.id=m.environment_id AND e.owner_user_id=m.user_id AND e.desired_state='active' AND e.revoked_at IS NULL WHERE m.id=$1 AND m.user_id=$2 AND m.state IN ('online','offline') AND m.seat_state='occupied' AND m.revoked_at IS NULL AND m.deleted_at IS NULL`, endpointID, userID).Scan(&gen)
		if err == sql.ErrNoRows || err == nil && gen != b.MachineGeneration {
			slog.Warn("peer network authority unavailable", "site", "network.go:801")
			return b, time.Time{}, ErrUnavailable
		}
		if err != nil {
			return b, time.Time{}, err
		}
	}
	b.WireGuardPublicKey = base64.RawURLEncoding.EncodeToString(key)
	if len(disco) == 32 {
		b.DiscoPublicKey = base64.RawURLEncoding.EncodeToString(disco)
	}
	b.QUICCertificateFingerprint = hex.EncodeToString(fingerprint)
	if len(quicPublic) != 32 {
		slog.Warn("peer network authority unavailable", "site", "network.go:813")
		return b, time.Time{}, ErrUnavailable
	}
	b.QUICPublicKey = base64.RawURLEncoding.EncodeToString(quicPublic)
	if _, err = netip.ParseAddr(b.VirtualAddress); err != nil {
		slog.Warn("peer network authority unavailable", "site", "network.go:817")
		return b, time.Time{}, ErrUnavailable
	}
	return b, expiry, nil
}

// maximumNetworkGrants bounds the access sessions one configuration carries. Past it the
// oldest are dropped, not the whole configuration: clients keep naming sessions that ended
// long ago as admitted, and one account's accumulated sessions must not stop its peers from
// refreshing their network. Live (unexpired) sessions are ordered first by the query.
const maximumRelayControlPeers = 16

const maximumNetworkGrants = 128

// Application access scopes (newest first) leave room for the durable machine mesh, so
// accumulated sessions can never push it out of the configuration.
const (
	maximumNetworkScopes       = 128
	maximumNetworkAccessScopes = maximumNetworkScopes - 32
	maximumNetworkAccessPeers  = 48
)

func boundGrants[T any](grants []T) []T {
	if len(grants) <= maximumNetworkGrants {
		return grants
	}
	slog.Warn("peer network configuration dropped the oldest access sessions", "kept", maximumNetworkGrants, "candidates", len(grants))
	return grants[:maximumNetworkGrants]
}

func bindingMap(b networkBinding) map[string]any {
	raw, _ := json.Marshal(b)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func validWireGuardPublicKey(key []byte) bool {
	if len(key) != curve25519.PointSize {
		return false
	}
	// X25519 rejects the low-order inputs that would produce an all-zero
	// shared secret. The scalar is validation-only and carries no identity.
	var scalar [curve25519.ScalarSize]byte
	for i := range scalar {
		scalar[i] = byte(i + 1)
	}
	_, err := curve25519.X25519(scalar[:], key)
	return err == nil
}

// keyedLocks serializes work per key within this process; entries are removed
// when their last holder leaves, so it never grows beyond the keys in use.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

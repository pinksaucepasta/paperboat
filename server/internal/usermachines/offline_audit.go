package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// PLAN 3b offline-capable authority, point 5 (F21-11): a host records every session it starts or refuses while the
// control plane is unreachable and uploads them on reconnect. Each event lands once (deduplicated by the host's
// event id) in the audit log of the workspace that decides that access, like an online access event, marked
// origin=offline. Managed SSH (PLAN 3b D1) has no server-side admission, so the host also reports every SSH session
// it starts, refuses and ends, online or offline (origin online|offline), through the same endpoint.
const (
	MaxOfflineAuditEvents = 500
	// OfflineAuditRetention is how long a host keeps an event it could not upload; older ones are refused.
	OfflineAuditRetention = 7 * 24 * time.Hour
)

var ErrOfflineAuditInvalid = errors.New("offline audit backlog is invalid")

// OfflineAuditEvent is one host-recorded offline session start or refusal.
type OfflineAuditEvent struct {
	EventID        string    `json:"event_id"`
	Kind           string    `json:"kind"`
	Capability     string    `json:"capability"`
	PeerEndpointID string    `json:"peer_endpoint_id"`
	PeerAccountID  string    `json:"peer_account_id"`
	OperationID    string    `json:"operation_id"`
	OccurredAt     time.Time `json:"occurred_at"`
	Reason         string    `json:"reason,omitempty"`
	// The fields below belong to the ssh_session_* kinds.
	Origin     string `json:"origin,omitempty"`
	Generation int64  `json:"generation,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	BytesIn    int64  `json:"bytes_in,omitempty"`
	BytesOut   int64  `json:"bytes_out,omitempty"`
}

type OfflineAuditReceipt struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
}

var offlineAuditIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// offlinePolicyCapability is the policy name machine_access_decision expects for each network capability.
var offlinePolicyCapability = map[string]string{"terminal": "terminal", "exec": "exec", "managed_ssh": "managed_ssh", "file_transfer": "files"}

var offlineRefusalReasons = map[string]bool{"no_scope": true, "backlog_full": true, "peer_unknown": true}
var sshRefusalReasons = map[string]bool{"busy": true, "no_access": true, "wrong_user": true, "not_set_up": true}
var sshEndOutcomes = map[string]bool{"closed": true, "revoked": true, "failed": true}

const sshCapability = "managed_ssh"

func isSSHKind(kind string) bool {
	return kind == "ssh_session_started" || kind == "ssh_session_refused" || kind == "ssh_session_ended"
}

// validSSHAuditEvent checks the ssh_session_* kinds: capability is implied, the operation id is optional, and an
// end names the started event it closes.
func validSSHAuditEvent(e OfflineAuditEvent) bool {
	if e.Capability != "" && e.Capability != sshCapability || e.Generation < 0 || e.BytesIn < 0 || e.BytesOut < 0 {
		return false
	}
	if e.Origin != "online" && e.Origin != "offline" || e.OperationID != "" && !offlineAuditIdentifier.MatchString(e.OperationID) {
		return false
	}
	switch e.Kind {
	case "ssh_session_started":
		return e.Reason == "" && e.Outcome == "" && e.SessionID == ""
	case "ssh_session_refused":
		return sshRefusalReasons[e.Reason] && e.Outcome == "" && e.SessionID == ""
	}
	return e.Reason == "" && sshEndOutcomes[e.Outcome] && offlineAuditIdentifier.MatchString(e.SessionID)
}

func validOfflineAuditEvent(e OfflineAuditEvent, now time.Time) bool {
	if !offlineAuditIdentifier.MatchString(e.EventID) || !offlineAuditIdentifier.MatchString(e.PeerEndpointID) || !offlineAuditIdentifier.MatchString(e.PeerAccountID) {
		return false
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.After(now.Add(time.Minute)) || e.OccurredAt.Before(now.Add(-OfflineAuditRetention)) {
		return false
	}
	if isSSHKind(e.Kind) {
		return validSSHAuditEvent(e)
	}
	if _, ok := offlinePolicyCapability[e.Capability]; !ok || !offlineAuditIdentifier.MatchString(e.OperationID) || e.Origin != "" {
		return false
	}
	switch e.Kind {
	case "offline_session_started":
		return e.Reason == ""
	case "offline_session_refused":
		return offlineRefusalReasons[e.Reason]
	}
	return false
}

// RecordOfflineAuditBacklog writes a host's offline session events. machineID and installationGeneration come from
// the verified machine proof that signed the whole upload.
func (s *Service) RecordOfflineAuditBacklog(ctx context.Context, machineID string, installationGeneration int64, events []OfflineAuditEvent) (OfflineAuditReceipt, error) {
	now := s.now().UTC()
	if machineID == "" || len(events) == 0 || len(events) > MaxOfflineAuditEvents {
		return OfflineAuditReceipt{}, ErrOfflineAuditInvalid
	}
	seen := make(map[string]bool, len(events))
	keys := make([]string, 0, len(events))
	accounts := make([]string, 0, len(events))
	for _, e := range events {
		if !validOfflineAuditEvent(e, now) || seen[e.EventID] {
			return OfflineAuditReceipt{}, ErrOfflineAuditInvalid
		}
		seen[e.EventID] = true
		keys = append(keys, "offline:"+e.EventID)
		accounts = append(accounts, e.PeerAccountID)
	}
	var receipt OfflineAuditReceipt
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var current bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machines WHERE id=$1 AND installation_generation=$2 AND revoked_at IS NULL AND deleted_at IS NULL)`, machineID, installationGeneration).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrNotFound
		}
		existing, err := stringSet(ctx, tx, `SELECT idempotency_key FROM paperboat.audit_events WHERE resource_type=ANY($1::text[]) AND resource_id=$2 AND idempotency_key=ANY($3::text[])`, []string{accessResource, "user_machine"}, machineID, keys)
		if err != nil {
			return err
		}
		known, err := stringSet(ctx, tx, `SELECT id FROM paperboat.users WHERE id=ANY($1::text[])`, accounts)
		if err != nil {
			return err
		}
		for _, e := range events {
			key := "offline:" + e.EventID
			if existing[key] {
				receipt.Duplicates++
				continue
			}
			eventType, result, metadata := auditEventFor(e, installationGeneration)
			// The host names the peer's account. The deciding workspace comes from that account's access to the
			// machine; an account that does not exist (anymore) cannot decide anything, so its event is filed under the
			// machine itself, in the machine's workspace, with the account in the metadata.
			actor, actorType, resource := "", audit.ActorSystem, "user_machine"
			if known[e.PeerAccountID] {
				actor, actorType, resource = e.PeerAccountID, audit.ActorUser, accessResource
			}
			if err := s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: actorType, EventType: eventType, Result: result, Reason: e.Reason,
				ResourceType: resource, ResourceID: machineID, IdempotencyKey: key, Metadata: metadata}); err != nil {
				return err
			}
			existing[key] = true
			receipt.Accepted++
		}
		return nil
	})
	return receipt, err
}

// auditEventFor maps a host event to its audit event type, result and metadata.
func auditEventFor(e OfflineAuditEvent, installationGeneration int64) (string, string, map[string]any) {
	capability, origin := offlinePolicyCapability[e.Capability], "offline"
	if isSSHKind(e.Kind) {
		capability, origin = sshCapability, e.Origin
	}
	metadata := map[string]any{"capability": capability, "scope_set": false, "origin": origin,
		"peer_endpoint_id": e.PeerEndpointID, "peer_account_id": e.PeerAccountID,
		"occurred_at": e.OccurredAt.UTC().Format(time.RFC3339Nano), "installation_generation": installationGeneration}
	if e.OperationID != "" {
		metadata["operation_id"] = e.OperationID
	}
	if e.Generation > 0 {
		metadata["scope_generation"] = e.Generation
	}
	if e.Kind == "ssh_session_started" || e.Kind == "offline_session_started" {
		metadata["session_id"] = e.EventID // an end names it, so the log pairs them
	}
	switch e.Kind {
	case "ssh_session_ended":
		metadata["session_id"], metadata["outcome"], metadata["bytes_in"], metadata["bytes_out"] = e.SessionID, e.Outcome, e.BytesIn, e.BytesOut
		return accessEndedEvent, audit.ResultSuccess, metadata
	case "offline_session_refused", "ssh_session_refused":
		return accessDeniedEvent, audit.ResultDenied, metadata
	}
	return accessStartedEvent, audit.ResultSuccess, metadata
}

func stringSet(ctx context.Context, tx *db.Tx, query string, args ...any) (map[string]bool, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v.String] = true
	}
	return out, rows.Err()
}

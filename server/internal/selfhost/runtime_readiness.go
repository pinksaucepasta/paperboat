package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrRuntimeReadinessUnavailable = errors.New("node readiness is unavailable")

type RuntimeReadiness struct {
	NodeID          string     `json:"node_id"`
	Capability      string     `json:"capability"`
	State           string     `json:"state"`
	Ready           bool       `json:"ready"`
	ProcessEpoch    string     `json:"process_epoch"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at"`
	ValidUntil      *time.Time `json:"valid_until"`
	ServerTime      time.Time  `json:"server_time"`
}

// RuntimeReadiness derives only the credential's node. It accepts no caller
// supplied resource selector and never returns credential material.
func (s *Service) RuntimeReadiness(ctx context.Context, credential string, staleAfter time.Duration) (RuntimeReadiness, error) {
	if !s.available() || staleAfter <= 0 {
		return RuntimeReadiness{}, ErrRuntimeReadinessUnavailable
	}
	if len(credential) < 32 || len(credential) > 4096 {
		return RuntimeReadiness{}, ErrUnauthorized
	}
	now := s.now().UTC()
	out := RuntimeReadiness{ServerTime: now}
	var heartbeat sql.NullTime
	err := s.database.SQL().QueryRowContext(ctx, `SELECT n.id,i.capability,n.state,n.ready,n.process_epoch,n.last_heartbeat_at
FROM paperboat.selfhost_installations i JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
WHERE i.runtime_hash=$1 AND i.state='active' AND i.claim_complete
AND(i.expires_at IS NULL OR i.expires_at>$2)`, digest(credential), now).Scan(&out.NodeID, &out.Capability, &out.State, &out.Ready, &out.ProcessEpoch, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeReadiness{}, ErrUnauthorized
	}
	if err != nil {
		return RuntimeReadiness{}, err
	}
	out.Ready = out.Ready && out.State == "ready" && out.ProcessEpoch != "" && heartbeat.Valid && heartbeat.Time.After(now.Add(-staleAfter))
	if heartbeat.Valid {
		at, until := heartbeat.Time.UTC(), heartbeat.Time.UTC().Add(staleAfter)
		out.LastHeartbeatAt, out.ValidUntil = &at, &until
	}
	return out, nil
}

package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
)

type ConfigComparisonReader func(context.Context, configsync.ConflictComparisonRequest) (configsync.ConflictComparison, error)

func (d *Dispatcher) configCompare(ctx context.Context, authorization Authorization, payload json.RawMessage) operation.Outcome {
	if d.config.ConfigCompare == nil {
		return failure("capability_required")
	}
	var request struct {
		Action string `json:"action"`
		configsync.ConflictComparisonRequest
	}
	if decodeStrict(payload, &request) != nil || request.Action != "compare" {
		return failure("invalid_request")
	}
	claims, ok := authorization.Value.(auth.Claims)
	if !ok || request.AssignmentID != authorization.ResourceID || request.AssignmentID != claims.AssignmentID || request.AssignmentVersion != claims.AssignmentVersion || request.Path != claims.ConfigPath || request.ConflictRevision != claims.ConflictRevision || request.ExpectedRemoteRevision != claims.ExpectedRemoteRevision {
		return failure("not_found_or_forbidden")
	}
	select {
	case d.compareGate <- struct{}{}:
	case <-ctx.Done():
		return failure("comparison_unavailable")
	}
	defer func() { <-d.compareGate }()
	value, err := d.config.ConfigCompare(ctx, request.ConflictComparisonRequest)
	defer clear(value.Local.Content)
	defer clear(value.Managed.Content)
	if errors.Is(err, configsync.ErrConflictComparisonStale) {
		return failure("conflict_stale")
	}
	if err != nil {
		return failure("comparison_unavailable")
	}
	return result(comparisonMetadata(value))
}

type comparisonSideMetadata struct {
	Present bool   `json:"present"`
	Kind    string `json:"kind"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
}
type comparisonMetadataResult struct {
	configsync.ConflictComparisonRequest
	Local   comparisonSideMetadata `json:"local"`
	Managed comparisonSideMetadata `json:"managed"`
}

func comparisonMetadata(value configsync.ConflictComparison) comparisonMetadataResult {
	side := func(s configsync.ConflictComparisonSide) comparisonSideMetadata {
		return comparisonSideMetadata{s.Present, s.Kind, s.SHA256, int64(len(s.Content))}
	}
	return comparisonMetadataResult{value.ConflictComparisonRequest, side(value.Local), side(value.Managed)}
}
func (d *Dispatcher) openConfigComparison(ctx context.Context, authorization Authorization, payload json.RawMessage, outcome operation.Outcome) (OutputStream, bool, error) {
	var metadata comparisonMetadataResult
	if json.Unmarshal(outcome.Result, &metadata) != nil || metadata.AssignmentID != authorization.ResourceID {
		return nil, false, ErrCredentialPolicy
	}
	select {
	case d.compareGate <- struct{}{}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	value, err := d.config.ConfigCompare(ctx, metadata.ConflictComparisonRequest)
	if err != nil || comparisonMetadata(value) != metadata {
		clear(value.Local.Content)
		clear(value.Managed.Content)
		<-d.compareGate
		return nil, false, &StreamError{Code: "conflict_stale"}
	}
	return &configComparisonStream{authorization: authorization, value: value, release: func() { <-d.compareGate }}, true, nil
}

type configComparisonStream struct {
	authorization Authorization
	value         configsync.ConflictComparison
	side          int
	offset        int
	once          sync.Once
	release       func()
}

func (s *configComparisonStream) Next(ctx context.Context) (protocol.BinaryFrame, error) {
	if err := ctx.Err(); err != nil {
		return protocol.BinaryFrame{}, err
	}
	if s.authorization.Revoked != nil && s.authorization.Revoked.Load() || !s.authorization.ExpiresAt.IsZero() && !time.Now().Before(s.authorization.ExpiresAt) {
		return protocol.BinaryFrame{}, &StreamError{Code: "credential_expired"}
	}
	for s.side < 2 {
		content := s.value.Local.Content
		channel := protocol.Stdout
		if s.side == 1 {
			content = s.value.Managed.Content
			channel = protocol.Stderr
		}
		if s.offset == len(content) {
			s.side++
			s.offset = 0
			continue
		}
		end := s.offset + (64 << 10)
		if end > len(content) {
			end = len(content)
		}
		frame := protocol.BinaryFrame{Channel: channel, StartSequence: uint64(s.offset), Data: content[s.offset:end]}
		s.offset = end
		return frame, nil
	}
	return protocol.BinaryFrame{}, &StreamEnd{Payload: json.RawMessage(`{"type":"comparison_complete"}`)}
}
func (s *configComparisonStream) Close() error {
	s.once.Do(func() {
		clear(s.value.Local.Content)
		clear(s.value.Managed.Content)
		s.value.Local.Content = nil
		s.value.Managed.Content = nil
		s.release()
	})
	return nil
}

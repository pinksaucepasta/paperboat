// Package managedssh owns server authority for managed SSH public keys and
// immutable machine host-key observations.
package managedssh

import (
	"context"
	"crypto/rand"
	"errors"
	"github.com/google/uuid"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

var (
	ErrConflict           = errors.New("managed SSH key conflicts with current authority")
	ErrUnavailable        = errors.New("managed SSH authority is unavailable")
	ErrMachineKeyCapacity = errors.New("machine exceeds the managed SSH client-key limit")
)

type ClientKey struct {
	Fingerprint           [32]byte
	UserID                string
	CLIClientSessionID    string
	Algorithm             string
	PublicKey             string
	State                 string
	ReconciliationVersion uint64
	CreatedAt             time.Time
	RevokedAt             time.Time
	RevocationReason      string
}

type RegisterClientRequest struct {
	OperationID        string
	UserID             string
	CLIClientSessionID string
	PublicKey          string
	Now                time.Time
}

type RevokeClientRequest struct {
	OperationID string
	ActorUserID string
	Fingerprint [32]byte
	Reason      string
	Now         time.Time
}

type ListClientKeysRequest struct {
	ActorUserID       string
	UserMachineID     string
	MachineGeneration uint64
}

type ClientKeySet struct {
	UserMachineID     string
	MachineGeneration uint64
	Keys              []ClientKey
}

type MachineTarget struct {
	UserMachineID         string
	MachineGeneration     uint64
	OSUser                string
	TargetPort            uint16
	ReconciliationVersion uint64
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type RegisterTargetRequest struct {
	OperationID       string
	ActorUserID       string
	UserMachineID     string
	MachineGeneration uint64
	OSUser            string
	TargetPort        uint16
	Now               time.Time
}

type UpdateTargetPortRequest struct {
	OperationID                   string
	ActorUserID                   string
	UserMachineID                 string
	MachineGeneration             uint64
	TargetPort                    uint16
	ExpectedReconciliationVersion uint64
	Now                           time.Time
}

type GetTargetRequest struct {
	ActorUserID       string
	UserMachineID     string
	MachineGeneration uint64
}

type HostKeySet struct {
	ID                    string
	UserMachineID         string
	MachineGeneration     uint64
	ObservationGeneration uint64
	Fingerprint           [32]byte
	State                 string
	ReconciliationVersion uint64
	Keys                  []wire.PublicKey
	ObservedAt            time.Time
	PromotedAt            time.Time
}

type ObserveHostRequest struct {
	OperationID           string
	UserID                string
	UserMachineID         string
	MachineGeneration     uint64
	ObservationGeneration uint64
	PublicKeys            []string
	Now                   time.Time
}

type PromoteHostRequest struct {
	OperationID         string
	ActorUserID         string
	UserMachineID       string
	MachineGeneration   uint64
	SetID               string
	ExpectedFingerprint [32]byte
	Now                 time.Time
}

type GetHostKeySetRequest struct {
	ActorUserID       string
	UserMachineID     string
	MachineGeneration uint64
}

type Repository interface {
	RegisterClient(context.Context, RegisterClientRequest, ClientKey) (ClientKey, error)
	RevokeClient(context.Context, RevokeClientRequest) (ClientKey, error)
	ListClientKeys(context.Context, ListClientKeysRequest) (ClientKeySet, error)
	ObserveHost(context.Context, ObserveHostRequest, HostKeySet) (HostKeySet, error)
	PromoteHost(context.Context, PromoteHostRequest) (HostKeySet, error)
	GetActiveHost(context.Context, GetHostKeySetRequest) (HostKeySet, error)
	GetPendingHost(context.Context, GetHostKeySetRequest) (HostKeySet, error)
	RegisterTarget(context.Context, RegisterTargetRequest) (MachineTarget, error)
	UpdateTargetPort(context.Context, UpdateTargetPortRequest) (MachineTarget, error)
	GetTarget(context.Context, GetTargetRequest) (MachineTarget, error)
}

func (s *Service) RegisterTarget(ctx context.Context, request RegisterTargetRequest) (MachineTarget, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || !validOSUser(request.OSUser) || request.TargetPort == 0 || request.Now.IsZero() {
		return MachineTarget{}, wire.ErrInvalid
	}
	request.Now = request.Now.UTC()
	return s.repository.RegisterTarget(ctx, request)
}

func (s *Service) UpdateTargetPort(ctx context.Context, request UpdateTargetPortRequest) (MachineTarget, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || request.TargetPort == 0 || request.ExpectedReconciliationVersion == 0 || request.ExpectedReconciliationVersion > math.MaxInt64 || request.Now.IsZero() {
		return MachineTarget{}, wire.ErrInvalid
	}
	request.Now = request.Now.UTC()
	return s.repository.UpdateTargetPort(ctx, request)
}

func (s *Service) GetTarget(ctx context.Context, request GetTargetRequest) (MachineTarget, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 {
		return MachineTarget{}, wire.ErrInvalid
	}
	return s.repository.GetTarget(ctx, request)
}

type Service struct {
	repository Repository
	random     io.Reader
}

func NewService(repository Repository) (*Service, error) {
	if repository == nil {
		return nil, wire.ErrInvalid
	}
	return &Service{repository: repository, random: rand.Reader}, nil
}

func (s *Service) RegisterClient(ctx context.Context, request RegisterClientRequest) (ClientKey, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.UserID) || !bounded(request.CLIClientSessionID) || request.Now.IsZero() {
		return ClientKey{}, wire.ErrInvalid
	}
	key, err := wire.ParsePublicKey(request.PublicKey, true)
	if err != nil {
		return ClientKey{}, err
	}
	return s.repository.RegisterClient(ctx, request, ClientKey{
		Fingerprint: key.Fingerprint, Algorithm: key.Algorithm, PublicKey: key.PublicKey,
		UserID: request.UserID, CLIClientSessionID: request.CLIClientSessionID,
		State: "active", ReconciliationVersion: 1, CreatedAt: request.Now.UTC(),
	})
}

func (s *Service) RevokeClient(ctx context.Context, request RevokeClientRequest) (ClientKey, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.ActorUserID) || request.Fingerprint == [32]byte{} || !validClientRevocation(request.Reason) || request.Now.IsZero() {
		return ClientKey{}, wire.ErrInvalid
	}
	request.Now = request.Now.UTC()
	return s.repository.RevokeClient(ctx, request)
}

func (s *Service) ListClientKeys(ctx context.Context, request ListClientKeysRequest) (ClientKeySet, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 {
		return ClientKeySet{}, wire.ErrInvalid
	}
	return s.repository.ListClientKeys(ctx, request)
}

func (s *Service) ObserveHost(ctx context.Context, request ObserveHostRequest) (HostKeySet, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.UserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || request.ObservationGeneration == 0 || request.ObservationGeneration > math.MaxInt64 || request.Now.IsZero() {
		return HostKeySet{}, wire.ErrInvalid
	}
	inventory, err := wire.ParseHostPublicKeys(request.PublicKeys)
	if err != nil {
		return HostKeySet{}, err
	}
	id, err := uuid.NewRandomFromReader(s.random)
	if err != nil {
		return HostKeySet{}, err
	}
	set := HostKeySet{ID: "keyset_" + id.String(), UserMachineID: request.UserMachineID, MachineGeneration: request.MachineGeneration, ObservationGeneration: request.ObservationGeneration, Fingerprint: inventory.Fingerprint, ReconciliationVersion: 1, Keys: inventory.Keys, ObservedAt: request.Now.UTC()}
	return s.repository.ObserveHost(ctx, request, set)
}

func (s *Service) PromoteHost(ctx context.Context, request PromoteHostRequest) (HostKeySet, error) {
	if s == nil || s.repository == nil || ctx == nil || !validOperationID(request.OperationID) || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 || !validHostKeySetID(request.SetID) || request.ExpectedFingerprint == [32]byte{} || request.Now.IsZero() {
		return HostKeySet{}, wire.ErrInvalid
	}
	request.Now = request.Now.UTC()
	return s.repository.PromoteHost(ctx, request)
}

func (s *Service) GetActiveHost(ctx context.Context, request GetHostKeySetRequest) (HostKeySet, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 {
		return HostKeySet{}, wire.ErrInvalid
	}
	return s.repository.GetActiveHost(ctx, request)
}

func (s *Service) GetPendingHost(ctx context.Context, request GetHostKeySetRequest) (HostKeySet, error) {
	if s == nil || s.repository == nil || ctx == nil || !bounded(request.ActorUserID) || !bounded(request.UserMachineID) || request.MachineGeneration == 0 || request.MachineGeneration > math.MaxInt64 {
		return HostKeySet{}, wire.ErrInvalid
	}
	return s.repository.GetPendingHost(ctx, request)
}

var hostKeySetIDPattern = regexp.MustCompile(`^keyset_[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func validHostKeySetID(value string) bool { return hostKeySetIDPattern.MatchString(value) }

func validOperationID(value string) bool {
	if len(value) < 16 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' || character == ' ' {
			return false
		}
	}
	return true
}

func validClientRevocation(value string) bool {
	switch value {
	case "client_revoked", "client_logout", "account_revoked", "key_rotated", "key_compromise":
		return true
	default:
		return false
	}
}

func bounded(value string) bool {
	return len(value) > 0 && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00")
}

func validOSUser(value string) bool {
	if value == "" || len(value) > 255 || value[0] == '-' || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00@") {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

package envinject

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/environmentkey"
)

type LayerConfig struct {
	Path, Issuer, AccountID, MachineID string
	InstallationGeneration             uint64
	Keys                               environmentkey.Source
	Marker                             environmentkey.LayerGenesisMarker
}
type layerFloor struct {
	ObservationSeq uint64                     `json:"observation_seq"`
	ObservedState  string                     `json:"observed_state"`
	Pending        *api.VaultLayerObservation `json:"pending,omitempty"`
	Generation     uint64                     `json:"generation"`
	DocumentID     string                     `json:"document_id"`
	Fence          uint64                     `json:"fence"`
	Epoch          uint64                     `json:"epoch"`
	Revision       uint64                     `json:"revision"`
	SourceID       string                     `json:"source_id"`
}
type layerHighWater struct {
	Schema                 string                `json:"schema"`
	AccountID              string                `json:"account_id"`
	MachineID              string                `json:"machine_id"`
	InstallationGeneration uint64                `json:"installation_generation"`
	HostKeyGeneration      uint64                `json:"host_key_generation"`
	HostPublic             string                `json:"host_public"`
	Floors                 map[string]layerFloor `json:"floors"`
}
type LayerStore struct {
	mu     sync.Mutex
	config LayerConfig
	write  func(string, []byte, any) error
}

func NewLayerStore(c LayerConfig) (*LayerStore, error) {
	if !filepath.IsAbs(c.Path) || c.Issuer == "" || !validIdentifier(c.AccountID) || !validIdentifier(c.MachineID) || c.InstallationGeneration == 0 || c.Keys == nil || c.Marker == nil {
		return nil, ErrInvalidSnapshot
	}
	return &LayerStore{config: c, write: writeLayerState}, nil
}
func layerCoordinate(s api.VaultLayerSource) string {
	return s.WorkspaceID + "\x00" + s.OwnerKind + "\x00" + s.OwnerID + "\x00" + s.MachineID
}

// Environment accepts only a fresh, machine-authenticated context. Values never
// enter persistent state; authenticated floors survive restart and cache loss.
func (s *LayerStore) Environment(ctx context.Context, binding LaunchContext, bundle api.VaultLayerContext) ([]string, error) {
	if ctx == nil || s == nil {
		return nil, ErrInvalidSnapshot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.config
	r := bundle.Recipient
	if bundle.WorkspaceID != binding.WorkspaceID || bundle.ActorAccountID != binding.ActorAccountID || bundle.MachineID != c.MachineID || r.RecipientAccount != c.AccountID || r.MachineID != c.MachineID || r.InstallationGeneration != c.InstallationGeneration || len(bundle.Layers) > 3 {
		return nil, ErrInvalidSnapshot
	}
	if binding.WorkspaceID == "personal" && binding.ActorAccountID != c.AccountID {
		return nil, ErrInvalidSnapshot
	}
	material, err := c.Keys.Load(ctx)
	if err != nil {
		return nil, err
	}
	defer material.Destroy()
	public, err := material.Public()
	if err != nil || r.HostKeyGeneration != material.Generation || r.HostPublic != base64.RawURLEncoding.EncodeToString(public[:]) {
		return nil, ErrInvalidSnapshot
	}
	integrity, err := material.StateIntegrityKey()
	if err != nil {
		return nil, err
	}
	defer clear(integrity[:])
	floor := layerHighWater{}
	established, err := c.Marker.LayerGenesisEstablished()
	if err != nil {
		return nil, err
	}
	err = readLayerState(c.Path, integrity[:], &floor)
	if errors.Is(err, os.ErrNotExist) {
		if established {
			return nil, ErrObservationLost
		}
		floor = layerHighWater{Schema: "paperboat.environment-layer-high-water/v1", AccountID: c.AccountID, MachineID: c.MachineID, InstallationGeneration: c.InstallationGeneration, HostKeyGeneration: material.Generation, HostPublic: r.HostPublic, Floors: map[string]layerFloor{}}
	} else if err != nil {
		return nil, ErrObservationLost
	}
	if !established && floor.Schema == "paperboat.environment-layer-high-water/v1" && floor.AccountID == c.AccountID && floor.MachineID == c.MachineID && floor.InstallationGeneration < c.InstallationGeneration && floor.HostKeyGeneration < material.Generation {
		floor = layerHighWater{Schema: "paperboat.environment-layer-high-water/v1", AccountID: c.AccountID, MachineID: c.MachineID, InstallationGeneration: c.InstallationGeneration, HostKeyGeneration: material.Generation, HostPublic: r.HostPublic, Floors: map[string]layerFloor{}}
	}
	if floor.Schema != "paperboat.environment-layer-high-water/v1" || floor.AccountID != c.AccountID || floor.MachineID != c.MachineID || floor.InstallationGeneration != c.InstallationGeneration || floor.HostKeyGeneration != material.Generation || floor.HostPublic != r.HostPublic || floor.Floors == nil || len(floor.Floors) > 1024 {
		return nil, ErrInvalidSnapshot
	}
	values := map[string][]byte{}
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	seen := map[int]bool{}
	layers := append([]api.VaultLayerDelivery(nil), bundle.Layers...)
	rank := func(source api.VaultLayerSource) (int, bool) {
		if source.WorkspaceID != binding.WorkspaceID {
			return 0, false
		}
		if source.OwnerKind == "team" {
			return 0, binding.WorkspaceID != "personal" && source.OwnerID == binding.WorkspaceID && source.MachineID == ""
		}
		if source.OwnerKind != "personal" || source.OwnerID != binding.ActorAccountID || binding.ActorAccountID != c.AccountID {
			return 0, false
		}
		if source.MachineID == "" {
			return 1, true
		}
		return 2, source.MachineID == c.MachineID
	}
	sort.Slice(layers, func(i, j int) bool { a, _ := rank(layers[i].Source); b, _ := rank(layers[j].Source); return a < b })
	for _, delivery := range layers {
		n, ok := rank(delivery.Source)
		if !ok || seen[n] || delivery.State != "ready" {
			return nil, ErrNotReady
		}
		seen[n] = true
		recipient := delivery.Recipient
		if recipient.RecipientAccount != r.RecipientAccount || recipient.MachineID != r.MachineID || recipient.InstallationGeneration != r.InstallationGeneration || recipient.HostKeyGeneration != r.HostKeyGeneration || recipient.HostPublic != r.HostPublic {
			return nil, ErrInvalidSnapshot
		}
		writer, err := base64.RawURLEncoding.Strict().DecodeString(delivery.WriterPublic)
		if err != nil {
			return nil, ErrInvalidSnapshot
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(delivery.Envelope)
		if err != nil {
			return nil, ErrInvalidSnapshot
		}
		layer, err := env.ParseVaultLayer(raw, writer)
		if err != nil {
			return nil, errors.Join(ErrInvalidSnapshot, s.recordLayerFailure(ctx, &floor, integrity[:], public[:], delivery))
		}
		claims := layer.Claims
		sourceID, err := env.ParseDocumentID(delivery.Source.DocumentID)
		if err != nil {
			return nil, ErrInvalidSnapshot
		}
		source := delivery.Source
		if claims.Issuer != c.Issuer || claims.RecipientAccount != r.RecipientAccount || claims.MachineID != r.MachineID || claims.InstallationGeneration != r.InstallationGeneration || claims.HostKeyGeneration != r.HostKeyGeneration || !bytes.Equal(claims.HostPublic, public[:]) || claims.DeliveryGeneration != recipient.DeliveryGeneration || claims.FenceGeneration != recipient.FenceGeneration || layer.ID.String() != recipient.DocumentID || claims.WriterAccount != delivery.WriterAccount || claims.Source.WorkspaceID != source.WorkspaceID || claims.Source.OwnerKind != source.OwnerKind || claims.Source.OwnerID != source.OwnerID || claims.Source.MachineID != source.MachineID || claims.Source.KeyEpoch != source.KeyEpoch || claims.Source.Revision != source.Revision || !bytes.Equal(claims.Source.Digest, sourceID[:]) {
			return nil, ErrInvalidSnapshot
		}
		coordinate := layerCoordinate(source)
		previous := floor.Floors[coordinate]
		if recipient.FenceGeneration < previous.Fence || recipient.DeliveryGeneration < previous.Generation || source.KeyEpoch < previous.Epoch || source.KeyEpoch == previous.Epoch && source.Revision < previous.Revision {
			return nil, ErrInvalidSnapshot
		}
		if recipient.DeliveryGeneration == previous.Generation && previous.Generation > 0 && recipient.DocumentID != previous.DocumentID {
			return nil, ErrInvalidSnapshot
		}
		if source.KeyEpoch == previous.Epoch && source.Revision == previous.Revision && previous.Revision > 0 && source.DocumentID != previous.SourceID {
			return nil, ErrInvalidSnapshot
		}
		opened, err := env.OpenVaultLayer(ctx, layer, claims, material.Private[:])
		if err != nil {
			if ctx.Err() == nil {
				err = errors.Join(err, s.recordLayerFailure(ctx, &floor, integrity[:], public[:], delivery))
			}
			return nil, err
		}
		for name, value := range opened {
			clear(values[name])
			values[name] = value
		}
		next := layerFloor{Generation: recipient.DeliveryGeneration, DocumentID: recipient.DocumentID, Fence: recipient.FenceGeneration, Epoch: source.KeyEpoch, Revision: source.Revision, SourceID: source.DocumentID, ObservationSeq: previous.ObservationSeq, ObservedState: previous.ObservedState, Pending: previous.Pending}
		if previous.Generation != next.Generation || previous.DocumentID != next.DocumentID || previous.Fence != next.Fence || previous.ObservedState != "applied" {
			keyID, err := env.KeyIDX25519(public[:])
			if err != nil {
				return nil, err
			}
			next.ObservationSeq++
			next.ObservedState = "applied"
			next.Pending = &api.VaultLayerObservation{WorkspaceID: source.WorkspaceID, OwnerKind: source.OwnerKind, OwnerID: source.OwnerID, SourceMachineID: source.MachineID, DeliveryGeneration: next.Generation, DocumentID: next.DocumentID, FenceGeneration: next.Fence, HostRecipientKeyID: keyID, ObservationSeq: next.ObservationSeq, State: "applied", ObservedAt: time.Now().UTC()}
		}
		floor.Floors[coordinate] = next
	}
	if len(values) > env.MaximumVariables {
		return nil, ErrInvalidSnapshot
	}
	bytesTotal := 0
	for name, value := range values {
		bytesTotal += len(name) + len(value) + 2
		if bytesTotal > env.MaximumScopeBytes {
			return nil, ErrInvalidSnapshot
		}
	}
	if len(floor.Floors) > 1024 {
		return nil, ErrInvalidSnapshot
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.write(c.Path, integrity[:], floor); err != nil {
		return nil, err
	}
	if !established {
		if err := c.Marker.EstablishLayerGenesis(); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+string(values[name]))
	}
	return result, nil
}

func (s *LayerStore) pendingState(ctx context.Context) (layerHighWater, []byte, error) {
	material, err := s.config.Keys.Load(ctx)
	if err != nil {
		return layerHighWater{}, nil, err
	}
	defer material.Destroy()
	integrity, err := material.StateIntegrityKey()
	if err != nil {
		return layerHighWater{}, nil, err
	}
	floor := layerHighWater{}
	if err := readLayerState(s.config.Path, integrity[:], &floor); err != nil {
		clear(integrity[:])
		return floor, nil, err
	}
	return floor, bytes.Clone(integrity[:]), nil
}
func (s *LayerStore) PendingObservations(ctx context.Context) ([]api.VaultLayerObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	floor, key, err := s.pendingState(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	coordinates := make([]string, 0, len(floor.Floors))
	for coordinate, value := range floor.Floors {
		if value.Pending != nil {
			coordinates = append(coordinates, coordinate)
		}
	}
	sort.Strings(coordinates)
	reports := []api.VaultLayerObservation{}
	for _, coordinate := range coordinates {
		reports = append(reports, *floor.Floors[coordinate].Pending)
	}
	return reports, nil
}
func (s *LayerStore) AcknowledgeObservations(ctx context.Context, reports []api.VaultLayerObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	floor, key, err := s.pendingState(ctx)
	if err != nil {
		return err
	}
	defer clear(key)
	for _, report := range reports {
		coordinate := report.WorkspaceID + "\x00" + report.OwnerKind + "\x00" + report.OwnerID + "\x00" + report.SourceMachineID
		value := floor.Floors[coordinate]
		if value.Pending != nil && value.Pending.ObservationSeq == report.ObservationSeq && value.Pending.DocumentID == report.DocumentID && value.Pending.FenceGeneration == report.FenceGeneration {
			value.Pending = nil
			floor.Floors[coordinate] = value
		}
	}
	return s.write(s.config.Path, key, floor)
}

func (s *LayerStore) recordLayerFailure(ctx context.Context, floor *layerHighWater, key, public []byte, delivery api.VaultLayerDelivery) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r, source := delivery.Recipient, delivery.Source
	if r.DeliveryGeneration == 0 || r.DeliveryGeneration > env.MaximumContractInteger || r.FenceGeneration == 0 || r.FenceGeneration > env.MaximumContractInteger || source.KeyEpoch == 0 || source.KeyEpoch > env.MaximumContractInteger || source.Revision == 0 || source.Revision > env.MaximumContractInteger {
		return ErrInvalidSnapshot
	}
	if _, err := env.ParseDocumentID(r.DocumentID); err != nil {
		return err
	}
	if _, err := env.ParseDocumentID(source.DocumentID); err != nil {
		return err
	}
	coordinate := layerCoordinate(source)
	previous := floor.Floors[coordinate]
	if r.FenceGeneration < previous.Fence || r.DeliveryGeneration < previous.Generation || source.KeyEpoch < previous.Epoch || source.KeyEpoch == previous.Epoch && source.Revision < previous.Revision {
		return ErrInvalidSnapshot
	}
	if previous.Generation == r.DeliveryGeneration && previous.DocumentID == r.DocumentID && previous.Fence == r.FenceGeneration && previous.SourceID == source.DocumentID && previous.ObservedState == "failed" {
		return nil
	}
	keyID, err := env.KeyIDX25519(public)
	if err != nil {
		return err
	}
	next := layerFloor{Generation: r.DeliveryGeneration, DocumentID: r.DocumentID, Fence: r.FenceGeneration, Epoch: source.KeyEpoch, Revision: source.Revision, SourceID: source.DocumentID, ObservationSeq: previous.ObservationSeq + 1, ObservedState: "failed"}
	code := "environment_projection_invalid"
	next.Pending = &api.VaultLayerObservation{WorkspaceID: source.WorkspaceID, OwnerKind: source.OwnerKind, OwnerID: source.OwnerID, SourceMachineID: source.MachineID, DeliveryGeneration: next.Generation, DocumentID: next.DocumentID, FenceGeneration: next.Fence, HostRecipientKeyID: keyID, ObservationSeq: next.ObservationSeq, State: "failed", ErrorCode: &code, ObservedAt: time.Now().UTC()}
	floor.Floors[coordinate] = next
	if len(floor.Floors) > 1024 {
		return ErrInvalidSnapshot
	}
	if err := s.write(s.config.Path, key, floor); err != nil {
		return err
	}
	return s.config.Marker.EstablishLayerGenesis()
}

func clearLayerValues(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}

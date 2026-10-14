package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type teamRecordRotationControl struct {
	*personalRotationControl
	requests        []api.VaultTeamRotate
	failAfterCommit bool
	totalLoss       bool
	snapshotCalls   int
}

func (c *teamRecordRotationControl) GetVaultSharing(_ context.Context, account string) (api.VaultSharingState, error) {
	if account != c.account {
		return api.VaultSharingState{}, errors.New("unexpected recipient")
	}
	raw, err := decodeVaultTestEnvelope(c.state.Envelope)
	if err != nil {
		return api.VaultSharingState{}, err
	}
	protection, err := env.PasswordVaultProtection(raw)
	if err != nil {
		return api.VaultSharingState{}, err
	}
	return api.VaultSharingState{AccountID: account, VaultGeneration: c.state.Generation, SharingPublic: base64.RawURLEncoding.EncodeToString(protection.SharingPublic), WriterPublic: base64.RawURLEncoding.EncodeToString(protection.WriterPublic)}, nil
}
func (c *teamRecordRotationControl) GetVaultTeam(ctx context.Context, team string) (api.VaultTeamState, error) {
	out, err := c.vaultScopesControl.GetVaultTeam(ctx, team)
	if err != nil {
		return out, err
	}
	out.RecordSequence = c.records[scopeControlKey("team", team, "")].Sequence
	if c.totalLoss {
		out.Scope.Envelope = "unavailable"
		out.Scope.WriterPublic = "unavailable"
		out.Members[0].GrantEpoch = 0
	}
	return out, nil
}
func (c *teamRecordRotationControl) VaultRecords(ctx context.Context, coordinate api.VaultLayerCoordinate, after uint64) (api.VaultRecordsResult, error) {
	c.snapshotCalls++
	if c.totalLoss {
		return api.VaultRecordsResult{}, errors.New("total loss must not fetch ciphertext")
	}
	return c.personalRotationControl.VaultRecords(ctx, coordinate, after)
}
func (c *teamRecordRotationControl) RotateVaultTeam(_ context.Context, team string, in api.VaultTeamRotate) (api.VaultTeamState, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	defer local.Clear()
	var journal api.VaultTeamRotate
	if local.Pending == nil || local.Operation == nil || json.Unmarshal(local.Operation.Request, &journal) != nil || !reflect.DeepEqual(journal, in) {
		return api.VaultTeamState{}, errors.New("rotation not exactly journaled")
	}
	c.requests = append(c.requests, in)
	if c.state.Envelope == in.VaultEnvelope {
		return c.teams[team], nil
	}
	prior := c.records[scopeControlKey("team", team, "")]
	if in.Records.ExpectedSequence != prior.Sequence {
		return api.VaultTeamState{}, errors.New("wrong record snapshot fence")
	}
	if !in.ConfirmTotalLoss {
		docs := []string{}
		for _, r := range prior.Records {
			if !r.Deleted {
				docs = append(docs, r.DocumentID)
			}
		}
		sort.Strings(docs)
		if !reflect.DeepEqual(docs, in.Records.DocumentIDs) || len(docs) != len(in.Records.Envelopes) {
			return api.VaultTeamState{}, errors.New("wrong live inventory")
		}
	} else if len(in.Records.Envelopes) != 0 || len(in.Records.DocumentIDs) != 0 {
		return api.VaultTeamState{}, errors.New("total loss retained ciphertext")
	}
	scope, err := c.scopeState("team", team, "", in.ScopeEnvelope)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	next := api.VaultRecordsResult{Sequence: 1, Records: []api.VaultRecordState{}}
	for _, envelope := range in.Records.Envelopes {
		state, err := rotationRecordState(envelope, c.writerPublic, 1)
		if err != nil {
			return api.VaultTeamState{}, err
		}
		next.Records = append(next.Records, state)
	}
	c.records[scopeControlKey("team", team, "")] = next
	c.scopes[scopeControlKey("team", team, "")] = scope
	raw, err := decodeVaultTestEnvelope(in.VaultEnvelope)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	c.state, err = passwordVaultState(raw)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	out := c.teams[team]
	out.Scope = scope
	out.KeyEpoch = scope.KeyEpoch
	out.Generation++
	out.RecordSequence = 1
	out.Members[0].GrantEpoch = scope.KeyEpoch
	c.teams[team] = out
	if c.failAfterCommit {
		c.failAfterCommit = false
		return api.VaultTeamState{}, errors.New("lost rotation response")
	}
	return out, nil
}

func TestTeamRecordRotationPreservesLiveValuesAndTotalLossSkipsCiphertext(t *testing.T) {
	for _, totalLoss := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "total_loss"}[totalLoss], func(t *testing.T) {
			ctx := context.Background()
			v, base, store := newVaultScopesFixture(t)
			control := &teamRecordRotationControl{personalRotationControl: &personalRotationControl{vaultScopesControl: base, records: map[string]api.VaultRecordsResult{}}, failAfterCommit: true}
			v.Client = control
			const team = "team_alpha"
			if err := v.CreateTeamAt(ctx, team, 1, 1); err != nil {
				t.Fatal(err)
			}
			if !totalLoss {
				if err := v.SetScopeVariable(ctx, "team", team, "", "PRESERVED", []byte("team-rotation-value")); err != nil {
					t.Fatal(err)
				}
				if err := v.SetScopeVariable(ctx, "team", team, "", "REMOVED", []byte("discarded")); err != nil {
					t.Fatal(err)
				}
				if err := v.RemoveScopeVariable(ctx, "team", team, "", "REMOVED"); err != nil {
					t.Fatal(err)
				}
			} else {
				local, keys := loadVaultKeysForScopeTest(t, store)
				for _, entry := range keys.Teams {
					clear(entry.Key)
				}
				keys.Teams = []env.VaultTeamKey{}
				payload, err := keys.MarshalBinary()
				keys.Clear()
				if err != nil {
					t.Fatal(err)
				}
				clear(local.Payload)
				local.Payload = payload
				if err := store.SavePasswordVault(local); err != nil {
					local.Clear()
					t.Fatal(err)
				}
				local.Clear()
				control.records[scopeControlKey("team", team, "")] = api.VaultRecordsResult{Sequence: 97}
				control.totalLoss = true
			}
			before, oldKeys := loadVaultKeysForScopeTest(t, store)
			defer before.Clear()
			defer oldKeys.Clear()
			calls := control.snapshotCalls
			if err := v.RotateTeam(ctx, team, nil, totalLoss); err == nil {
				t.Fatal("lost commit response hidden")
			}
			if err := v.Resume(ctx); err != nil {
				t.Fatal(err)
			}
			if len(control.requests) != 2 || !reflect.DeepEqual(control.requests[0], control.requests[1]) {
				t.Fatal("retry changed ciphertext")
			}
			after, nextKeys := loadVaultKeysForScopeTest(t, store)
			defer after.Clear()
			defer nextKeys.Clear()
			if after.Operation != nil || after.Pending != nil || after.Head.Generation != before.Head.Generation+1 {
				t.Fatal("vault successor not committed")
			}
			out := control.teams[team]
			anchor, err := out.Scope.Decode()
			if err != nil {
				t.Fatal(err)
			}
			root, epoch, err := scopeKey(&nextKeys, "team", team, v.AccountID)
			if err != nil || epoch != 2 {
				t.Fatal("new team key missing", err)
			}
			values, err := env.OpenVaultScope(ctx, anchor, root)
			if err != nil || len(values) != 0 {
				clearVaultValues(values)
				t.Fatal("anchor contains values", err)
			}
			clearVaultValues(values)
			result := control.records[scopeControlKey("team", team, "")]
			if totalLoss {
				if len(result.Records) != 0 || control.snapshotCalls != calls || control.requests[0].Records.ExpectedSequence != 97 {
					t.Fatal("total loss fetched/retained old ciphertext")
				}
				return
			}
			if len(result.Records) != 1 || len(control.requests[0].Records.DocumentIDs) != 1 {
				t.Fatal("tombstone preserved or live record lost")
			}
			newKey, err := v.recordKey(&nextKeys, anchor)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(newKey)
			oldAnchor := anchor
			oldAnchor.Claims.KeyEpoch = 1
			oldKey, err := v.recordKey(&oldKeys, oldAnchor)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(oldKey)
			record, err := result.Records[0].Decode()
			if err != nil {
				t.Fatal(err)
			}
			name, value, err := env.OpenVaultRecord(ctx, record, newKey)
			if err != nil || name != "PRESERVED" || !bytes.Equal(value, []byte("team-rotation-value")) || record.Claims.Revision != 1 || record.Claims.WriterVaultGeneration != before.Head.Generation {
				clear(value)
				t.Fatal("value or writer metadata changed", err)
			}
			clear(value)
			if _, value, err := env.OpenVaultRecord(ctx, record, oldKey); err == nil {
				clear(value)
				t.Fatal("old epoch decrypts new record")
			}
		})
	}
}

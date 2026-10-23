package teams

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// Machine tags as a first-class action (`pb machine tag`, the console's "Edit tags"): each
// change edits the policy file's tags map and is saved as a new version, with the same rights,
// access-change review and audit as any policy save.

// ErrPolicyMachineNotFound: the machine is not in the workspace (not shared into the team, or
// another account's).
var ErrPolicyMachineNotFound = errors.New("machine is not in this workspace")

type MachineTagsRequest struct {
	Tags            []string `json:"tags"`
	ExpectedVersion uint64   `json:"expected_version"`
	Confirm         string   `json:"confirm,omitempty"`
}

type MachineTagsView struct {
	Tags          []string `json:"tags"`
	PolicyVersion uint64   `json:"policy_version"`
}

type WorkspaceTag struct {
	Name     string   `json:"name"`
	Machines []string `json:"machines"`
}

type WorkspaceTags struct {
	Tags          []WorkspaceTag `json:"tags"`
	CanEdit       string         `json:"can_edit"`
	PolicyVersion uint64         `json:"policy_version"`
}

// checkTagNames refuses a tag list that is not tag:<name> names; paths index the request list.
func checkTagNames(tags []string) ([]string, error) {
	var errs policy.Errors
	out := []string{}
	for i, t := range tags {
		name, ok := strings.CutPrefix(t, "tag:")
		if !ok || !policy.ValidName(name) {
			errs = append(errs, policy.Error{Path: "/tags/" + strconv.Itoa(i), Code: "invalid_name", Message: "a tag is named tag:<name> (lowercase letters, digits, '.', '_', '-')"})
			continue
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(errs) > 0 {
		return nil, &PolicyInvalidError{Errors: errs}
	}
	slices.Sort(out)
	return out, nil
}

// SetMachineTags makes one machine of the workspace carry exactly r.Tags.
func (s *Service) SetMachineTags(ctx context.Context, account, ws, machineID string, r MachineTagsRequest) (MachineTagsView, error) {
	tags, err := checkTagNames(r.Tags)
	if err != nil {
		return MachineTagsView{}, err
	}
	var name string
	view, err := s.writePolicy(ctx, account, ws, func(ctx context.Context, tx *db.Tx, w Workspace, cur storedPolicy) (policySave, error) {
		inv, err := policyInventoryTx(ctx, tx, w)
		if err != nil {
			return policySave{}, err
		}
		if !slices.ContainsFunc(inv.Machines, func(m policy.Machine) bool { return m.ID == machineID }) {
			return policySave{}, ErrPolicyMachineNotFound
		}
		name = "machine:" + inv.MachineName(machineID)
		next, err := policy.SetMachineTags(external(cur.source, inv), name, tags)
		if err != nil {
			return policySave{}, err
		}
		return policySave{actor: account, source: next, expected: r.ExpectedVersion, message: "Tags of " + strings.TrimPrefix(name, "machine:") + " changed", confirm: r.Confirm, kind: "saved"}, nil
	})
	if err != nil {
		return MachineTagsView{}, err
	}
	d, _ := policy.Parse([]byte(view.Policy))
	return MachineTagsView{Tags: policy.MachineTags(d, name), PolicyVersion: view.Version}, nil
}

// WorkspaceTagList lists the workspace's tags with their machines (IDs); anyone who may read the
// policy may read it.
func (s *Service) WorkspaceTagList(ctx context.Context, account, ws string) (WorkspaceTags, error) {
	var out WorkspaceTags
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		sc, err := scopeTx(ctx, tx, account, ws)
		if err != nil {
			return err
		}
		p, err := policyTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		inv, err := policyInventoryTx(ctx, tx, sc.w)
		if err != nil {
			return err
		}
		ids := map[string]string{}
		for id, n := range inv.MachineNames() {
			ids["machine:"+n] = id
		}
		d, _ := policy.Parse(external(p.source, inv))
		out = WorkspaceTags{Tags: []WorkspaceTag{}, CanEdit: sc.canEdit, PolicyVersion: p.version}
		for name, entries := range d.Tags {
			t := WorkspaceTag{Name: name, Machines: []string{}}
			for _, e := range entries {
				if id := ids[e]; id != "" {
					t.Machines = append(t.Machines, id)
				}
			}
			slices.Sort(t.Machines)
			out.Tags = append(out.Tags, t)
		}
		slices.SortFunc(out.Tags, func(a, b WorkspaceTag) int { return strings.Compare(a.Name, b.Name) })
		return nil
	})
	return out, err
}

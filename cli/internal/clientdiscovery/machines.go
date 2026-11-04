// Package clientdiscovery resolves exact authorized inventory identities.
package clientdiscovery

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/pinksaucepasta/paperboat/shared/client/api"
)

// Machine is an authorized inventory identity. ID remains stable when its name changes.
type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"alias"`
	Online   bool   `json:"online"`
	Platform string `json:"platform"`
}

type MachinePage struct {
	Items []Machine
	Total int
}

// AmbiguousMachineError means more than one authorized machine has the exact name.
type AmbiguousMachineError struct{ Name string }

func (e *AmbiguousMachineError) Error() string {
	return fmt.Sprintf("more than one authorized machine is named %q; use a machine ID", e.Name)
}

var ErrMachineNotFound = errors.New("machine is unavailable or access is denied")

func Machines(ctx context.Context, request api.Request) (MachinePage, error) {
	items, total, err := api.CollectOffsetInventoryPage[Machine](ctx, request, "/v1/machines", "items", url.Values{"sort": {"alias"}}, 0)
	if err != nil {
		return MachinePage{}, err
	}
	seen := make(map[string]bool, len(items))
	for _, m := range items {
		if m.ID == "" || m.Name == "" || seen[m.ID] {
			return MachinePage{}, errors.New("paperboat-server returned an invalid machine inventory")
		}
		seen[m.ID] = true
	}
	return MachinePage{Items: items, Total: total}, nil
}

// Resolve resolves once; callers retain the returned ID throughout any operation or retry.
func Resolve(ctx context.Context, request api.Request, idOrName string) (Machine, error) {
	page, err := Machines(ctx, request)
	if err != nil {
		return Machine{}, err
	}
	for _, m := range page.Items {
		if m.ID == idOrName {
			return m, nil
		}
	}
	var result Machine
	found := false
	for _, m := range page.Items {
		if m.Name != idOrName {
			continue
		}
		if found {
			return Machine{}, &AmbiguousMachineError{Name: idOrName}
		}
		result, found = m, true
	}
	if !found {
		return Machine{}, ErrMachineNotFound
	}
	return result, nil
}

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func TestFreshenSSHHostKeys(t *testing.T) {
	old := refreshInventoryForSSHHostKeys
	t.Cleanup(func() { refreshInventoryForSSHHostKeys = old })
	withKeys := api.UserMachine{Alias: "m", SSHHostKeys: []string{"ssh-ed25519 AAAA"}}
	cases := []struct {
		name          string
		start         api.UserMachine
		refreshErr    error
		resolved      api.UserMachine
		wantRefreshes int
		wantResolves  int
		wantKeys      int
		wantNone      bool
	}{
		{name: "keys known: nothing is asked", start: withKeys, wantKeys: 1},
		{name: "keys published since the last refresh: found by one refresh", start: api.UserMachine{Alias: "m"}, resolved: withKeys, wantRefreshes: 1, wantResolves: 1, wantKeys: 1},
		{name: "still none after the refresh: named", start: api.UserMachine{Alias: "m"}, resolved: api.UserMachine{Alias: "m"}, wantRefreshes: 1, wantResolves: 1, wantNone: true},
		{name: "refresh failed: not resolved again, named", start: api.UserMachine{Alias: "m"}, refreshErr: errors.New("daemon down"), resolved: withKeys, wantRefreshes: 1, wantNone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refreshes, resolves := 0, 0
			refreshInventoryForSSHHostKeys = func(context.Context) error { refreshes++; return tc.refreshErr }
			got, none := freshenSSHHostKeys(context.Background(), tc.start, func() (api.UserMachine, error) { resolves++; return tc.resolved, nil })
			if refreshes != tc.wantRefreshes || resolves != tc.wantResolves || len(got.SSHHostKeys) != tc.wantKeys || none != tc.wantNone {
				t.Fatalf("refreshes=%d resolves=%d keys=%d none=%v, want %d %d %d %v", refreshes, resolves, len(got.SSHHostKeys), none, tc.wantRefreshes, tc.wantResolves, tc.wantKeys, tc.wantNone)
			}
		})
	}
}

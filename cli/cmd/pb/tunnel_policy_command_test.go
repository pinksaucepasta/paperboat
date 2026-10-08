package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

type fakeLazyPolicyClient struct {
	current           api.LazyPolicy
	upsert            api.LazyPolicyUpsertRequest
	deletedGeneration int64
}

func (f *fakeLazyPolicyClient) GetLazyPolicy(context.Context, string) (api.LazyPolicy, error) {
	return f.current, nil
}
func (f *fakeLazyPolicyClient) UpsertLazyPolicy(_ context.Context, in api.LazyPolicyUpsertRequest) (api.LazyPolicy, error) {
	f.upsert = in
	out := f.current
	out.Generation = in.ExpectedGeneration + 1
	out.AccessMode, out.ExpiresAt = in.AccessMode, in.ExpiresAt
	return out, nil
}
func (f *fakeLazyPolicyClient) DeleteLazyPolicy(_ context.Context, _ string, generation int64) error {
	f.deletedGeneration = generation
	return nil
}

func runLazyPolicyCommand(t *testing.T, fake *fakeLazyPolicyClient, command *cobra.Command, args ...string) string {
	t.Helper()
	previous := lazyPolicyClientForCommand
	lazyPolicyClientForCommand = func(*cobra.Command) (lazyPolicyClient, error) { return fake, nil }
	t.Cleanup(func() { lazyPolicyClientForCommand = previous })
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs(args)
	command.SetContext(context.Background())
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestTunnelPolicyAllowUsesExplicitGenerationAndExplainsPortOwnership(t *testing.T) {
	fake := &fakeLazyPolicyClient{current: api.LazyPolicy{ID: "lap_1", Hostname: "p3000-env.example.test", MachineID: "machine_1", Generation: 4, Target: api.LazyPolicyTarget{Scheme: "http", Address: "127.0.0.1:3000"}, AccessMode: "private", OwnershipMode: "persistent_port", ExpiresAt: time.Now().Add(time.Hour)}}
	out := runLazyPolicyCommand(t, fake, tunnelPolicyAllowCommand(), "--id", "lap_1", "--generation", "4", "--access", "team", "machine_1", "3000")
	if fake.upsert.ExpectedGeneration != 4 || fake.upsert.Target.Address != "127.0.0.1:3000" || fake.upsert.AccessMode != "team" || !strings.Contains(out, "replacing the listener") || !strings.Contains(out, "will not start") {
		t.Fatalf("upsert=%+v output=%q", fake.upsert, out)
	}
}

func TestTunnelPolicyRevokeReadsCurrentGeneration(t *testing.T) {
	fake := &fakeLazyPolicyClient{current: api.LazyPolicy{ID: "lap_1", Generation: 9}}
	runLazyPolicyCommand(t, fake, tunnelPolicyRevokeCommand(), "lap_1")
	if fake.deletedGeneration != 9 {
		t.Fatalf("deleted generation=%d", fake.deletedGeneration)
	}
}

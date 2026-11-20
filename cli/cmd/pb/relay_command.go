package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/cli/internal/auth"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/spf13/cobra"
)

// relayListResult is one Paperboat relay region (no relay id: its servers are not shown) or one of the
// owner's own relays, which keep their id and name.
type relayListResult struct {
	RelayID    string `json:"relay_id,omitempty"`
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	ObservedAt int64  `json:"observed_at,omitempty"`
	// This machine's best connect time to the region's relays (under an hour old); absent until measured.
	LatencyMillis *int64 `json:"latency_ms,omitempty"`
}

func actionRelayList(command *cobra.Command, _ []string) error {
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		return err
	}
	if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			return err
		}
	}
	source, err := sessionauth.NewSource(cfg)
	if err != nil {
		return err
	}
	credential, err := source.Credential()
	if err != nil {
		return err
	}
	client := api.New(cfg.ServerURL, credential, nil)
	selfhost, pool, err := client.SelfhostInventory(command.Context(), "relay")
	if err != nil {
		return friendlyCommandError(err)
	}
	_, _, err = startLocalDaemonAndWait(command.Context(), cfg, installLocalDaemonService)
	if err != nil {
		return err
	}
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return err
	}
	local, err := localapi.NewClient(paths.SocketPath, 45*time.Second)
	if err != nil {
		return err
	}
	inventory, err := local.RelayInventory(command.Context())
	if err != nil {
		return fmt.Errorf("verified native relay inventory: %w", err)
	}
	hosted, err := client.ListHostedRelays(command.Context())
	if err != nil {
		return friendlyCommandError(err)
	}
	names := make(map[string]string, len(hosted.Items))
	for _, region := range hosted.Items {
		names[region.ID] = region.Name
	}
	results := relayListResults(inventory, selfhost, names, pool.Mode, time.Now())
	if err := addRelayLatency(command.Context(), client, results); err != nil {
		fmt.Fprintf(command.ErrOrStderr(), "Latency is not shown: %v\n", err)
	}
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(struct {
			Schema   string            `json:"schema"`
			PoolMode string            `json:"pool_mode"`
			Relays   []relayListResult `json:"relays"`
		}{Schema: "paperboat.relay-list/v1", PoolMode: pool.Mode, Relays: results})
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "REGION\tNAME\tSOURCE\tSTATUS\tLATENCY")
	for _, result := range results {
		region := result.Region
		if region == "" {
			region = "-"
		}
		latency := "-"
		if result.LatencyMillis != nil {
			latency = fmt.Sprintf("%d ms", *result.LatencyMillis)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", region, result.Name, result.Source, result.Status, latency)
	}
	return writer.Flush()
}

// relayListResults shows each Paperboat relay region once (ready while any of its relays is; names come
// from regionNames, the region id when unknown) and each of the owner's own relays by name.
func relayListResults(inventory localapi.RelayInventory, selfhost []api.SelfhostInstallation, regionNames map[string]string, poolMode string, now time.Time) []relayListResult {
	metadata := make(map[string]api.SelfhostInstallation, len(selfhost))
	for _, installation := range selfhost {
		metadata[installation.NodeID] = installation
	}
	results := make([]relayListResult, 0, len(inventory.Candidates)+len(selfhost))
	seen := make(map[string]bool, len(inventory.Candidates))
	regions := map[string]int{}
	nowUnix := now.Unix()
	for _, candidate := range inventory.Candidates {
		if !slices.Contains(candidate.Roles, "relay") || !slices.Contains(candidate.Transports, "derp_quic") {
			continue
		}
		installation, isSelfhost := metadata[candidate.NodeID]
		if poolMode == "self-hosted-only" && (!isSelfhost || installation.ScopeKind == "global" || !installation.Selected) {
			continue
		}
		ready := candidate.State == "ready" && candidate.ObservedAt <= nowUnix && nowUnix-candidate.ObservedAt <= 15 && candidate.ExpiresAt > nowUnix
		status := "unavailable"
		if ready {
			status = "reported_ready"
		}
		seen[candidate.NodeID] = true
		if isSelfhost && installation.ScopeKind != "global" {
			results = append(results, relayListResult{RelayID: candidate.NodeID, Name: installation.Name, Region: candidate.Region, Source: "self-hosted", Status: status, ObservedAt: candidate.ObservedAt})
			continue
		}
		i, ok := regions[candidate.Region]
		if !ok {
			name := cmp.Or(regionNames[candidate.Region], candidate.Region)
			i = len(results)
			regions[candidate.Region] = i
			results = append(results, relayListResult{Name: name, Region: candidate.Region, Source: "paperboat", Status: "unavailable"})
		}
		if ready {
			results[i].Status, results[i].ObservedAt = status, max(results[i].ObservedAt, candidate.ObservedAt)
		}
	}
	for _, installation := range selfhost {
		if installation.Selected && installation.ScopeKind != "global" && !seen[installation.NodeID] {
			results = append(results, relayListResult{RelayID: installation.NodeID, Name: installation.Name, Region: installation.Region, Source: "self-hosted", Status: "unavailable"})
		}
	}
	return results
}

// addRelayLatency fills in this machine's latest connect time to each relay, as its daemon reported it to the server.
func addRelayLatency(ctx context.Context, client *api.Client, results []relayListResult) error {
	machineID, err := configuredMachineID()
	if err != nil {
		return err
	}
	items, err := client.MachineLatencies(ctx, machineID)
	if err != nil {
		return err
	}
	setRelayLatency(results, items)
	return nil
}

// setRelayLatency gives each row its region's best time: Paperboat's regions from Paperboat's relays, the
// owner's own relays from the owner's own servers of that region.
func setRelayLatency(results []relayListResult, items []api.MachineLatency) {
	type key struct {
		region     string
		selfHosted bool
	}
	best := make(map[key]int64, len(items))
	for _, item := range items {
		if item.Role == "relay" {
			best[key{item.Region, item.SelfHosted}] = item.RTTMillis
		}
	}
	for i := range results {
		if rtt, ok := best[key{results[i].Region, results[i].Source == "self-hosted"}]; ok {
			results[i].LatencyMillis = &rtt
		}
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/spf13/cobra"
)

type relayListResult struct {
	RelayID    string `json:"relay_id"`
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	ObservedAt int64  `json:"observed_at,omitempty"`
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
	selfhost, pool, err := api.New(cfg.ServerURL, credential, nil).SelfhostInventory(command.Context(), "relay")
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
	results := relayListResults(inventory, selfhost, pool.Mode, time.Now())
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(struct {
			Schema   string            `json:"schema"`
			PoolMode string            `json:"pool_mode"`
			Relays   []relayListResult `json:"relays"`
		}{Schema: "paperboat.relay-list/v1", PoolMode: pool.Mode, Relays: results})
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RELAY\tNAME\tSOURCE\tREGION\tSTATUS")
	for _, result := range results {
		region := result.Region
		if region == "" {
			region = "-"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", result.RelayID, result.Name, result.Source, region, result.Status)
	}
	return writer.Flush()
}

func relayListResults(inventory localapi.RelayInventory, selfhost []api.SelfhostInstallation, poolMode string, now time.Time) []relayListResult {
	metadata := make(map[string]api.SelfhostInstallation, len(selfhost))
	for _, installation := range selfhost {
		metadata[installation.NodeID] = installation
	}
	results := make([]relayListResult, 0, len(inventory.Candidates)+len(selfhost))
	seen := make(map[string]bool, len(inventory.Candidates))
	nowUnix := now.Unix()
	for _, candidate := range inventory.Candidates {
		if !slices.Contains(candidate.Roles, "relay") || !slices.Contains(candidate.Transports, "derp_quic") {
			continue
		}
		installation, isSelfhost := metadata[candidate.NodeID]
		if poolMode == "self-hosted-only" && (!isSelfhost || installation.ScopeKind == "global" || !installation.Selected) {
			continue
		}
		result := relayListResult{RelayID: candidate.NodeID, Name: candidate.NodeID, Region: candidate.Region, Source: "paperboat", Status: "unavailable", ObservedAt: candidate.ObservedAt}
		if candidate.State == "ready" && candidate.ObservedAt <= nowUnix && nowUnix-candidate.ObservedAt <= 15 && candidate.ExpiresAt > nowUnix {
			result.Status = "reported_ready"
		}
		if isSelfhost {
			result.Name = installation.Name
			if installation.ScopeKind != "global" {
				result.Source = "self-hosted"
			}
		}
		results = append(results, result)
		seen[candidate.NodeID] = true
	}
	for _, installation := range selfhost {
		if installation.Selected && installation.ScopeKind != "global" && !seen[installation.NodeID] {
			results = append(results, relayListResult{RelayID: installation.NodeID, Name: installation.Name, Source: "self-hosted", Status: "unavailable"})
		}
	}
	return results
}

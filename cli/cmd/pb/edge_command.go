package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type edgeListItem struct {
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"`
	Source        string     `json:"source"`
	Region        string     `json:"region,omitempty"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat_at,omitempty"`
}

func edgeCommand() *cobra.Command {
	root := &cobra.Command{Use: "edge", Short: "Inspect tunnel edges", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	list := &cobra.Command{Use: "list", Short: "List managed or selected self-hosted tunnel edges", Long: `List tunnel edge nodes for the active workspace. Managed-only mode lists
Paperboat-managed edges. Self-hosted-only mode lists only selected self-hosted
edges, including ones that are currently unavailable. It never falls back to
managed edges. Self-hosted browser publications require a verified customer
domain and a ready certificate.

Listing an edge never guarantees admission for a particular route; domain and
route authorization still apply.

The table identifies each edge's source, region, status, and last heartbeat.
Self-hosted readiness comes from the control plane; hosted status is an
	observed node state. Use --json for paperboat.edge-list/v1 output with the
active workspace's pool mode and edge list.`, Example: "  pb edge list\n  pb edge list --workspace acme\n  pb edge list --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, err := backendClient(actionContext(command, args))
		if err != nil {
			return err
		}
		workspace, err := effectiveWorkspace(command)
		if err != nil {
			return err
		}
		scopeKind, scopeID := "account", ""
		if workspace != "personal" {
			scopeKind, scopeID = "team", workspace
		}
		selfhost, pool, err := client.SelfhostTunnelInventory(command.Context(), scopeKind, scopeID)
		if err != nil {
			return friendlyCommandError(err)
		}
		selected := make([]api.SelfhostInstallation, 0, len(selfhost))
		metadata := make(map[string]api.SelfhostInstallation, len(selfhost))
		for _, installation := range selfhost {
			metadata[installation.NodeID] = installation
			if installation.Selected && installation.ScopeKind != "global" {
				selected = append(selected, installation)
			}
		}
		showHosted := pool.Mode == "managed-only"
		items := make([]edgeListItem, 0, len(selfhost))
		observedAt := time.Now().UTC()
		if showHosted {
			page, err := client.ListHostedEdges(command.Context())
			if err != nil {
				return friendlyCommandError(err)
			}
			observedAt = page.ObservedAt
			for _, edge := range page.Items {
				items = append(items, edgeListItem{ID: edge.ID, Name: metadata[edge.ID].Name, Source: "paperboat", Region: edge.Region, Status: edge.Status, LastHeartbeat: edge.LastHeartbeat})
			}
		}
		for _, installation := range selected {
			status := "unavailable"
			if installation.Ready {
				status = "ready"
			}
			items = append(items, edgeListItem{ID: installation.NodeID, Name: installation.Name, Source: "self-hosted", Status: status})
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema": "paperboat.edge-list/v1", "observed_at": observedAt, "pool_mode": pool.Mode, "edges": items})
		}
		if len(items) == 0 {
			message := "No tunnel edges are available in the active workspace."
			if pool.Mode == "self-hosted-only" {
				message = "No selected self-hosted tunnel edge is visible in the active workspace. Managed edges are not used in self-hosted-only mode."
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), message)
			return err
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "EDGE\tNAME\tSOURCE\tREGION\tSTATUS\tLAST HEARTBEAT")
		for _, edge := range items {
			name, region, heartbeat := edge.Name, edge.Region, "-"
			if name == "" {
				name = "-"
			}
			if region == "" {
				region = "-"
			}
			if edge.LastHeartbeat != nil {
				heartbeat = edge.LastHeartbeat.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", edge.ID, name, edge.Source, region, edge.Status, heartbeat)
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		return err
	}}
	list.Flags().Bool("json", false, "print JSON")
	root.AddCommand(list)
	return root
}

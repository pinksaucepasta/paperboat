package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
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
	list := &cobra.Command{Use: "list", Short: "List hosted and self-hosted tunnel edges", Long: `List tunnel edge nodes visible to the signed-in account. Paperboat-hosted
edges and self-hosted edges selected in the account's tunnel pool appear in
mixed mode. Self-hosted-only mode shows selected self-hosted edges, including
ones that are currently unavailable.

If self-hosted-only mode has no selected active edge, hosted edges are shown
for reference. They cannot serve routes until the tunnel pool changes to
mixed mode. Listing an edge never guarantees admission for a particular route;
domain and route authorization still apply.

The table identifies each edge's source, region, status, and last heartbeat.
Self-hosted readiness comes from the control plane; hosted status is an
observed node state. Use --json for paperboat.edge-list/v1 output with the
pool mode, edges, and hosted_fallback_display_only marker.`, Example: "  pb edge list\n  pb edge list --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
		client, err := backendClient(actionContext(command, args))
		if err != nil {
			return err
		}
		selfhost, pool, err := client.SelfhostInventory(command.Context(), "tunnel")
		if err != nil {
			return friendlyCommandError(err)
		}
		showHosted := pool.Mode == "mixed" || len(selfhost) == 0
		items := make([]edgeListItem, 0, len(selfhost))
		observedAt := time.Now().UTC()
		if showHosted {
			page, err := client.ListHostedEdges(command.Context())
			if err != nil {
				return friendlyCommandError(err)
			}
			observedAt = page.ObservedAt
			for _, edge := range page.Items {
				items = append(items, edgeListItem{ID: edge.ID, Source: "paperboat", Region: edge.Region, Status: edge.Status, LastHeartbeat: edge.LastHeartbeat})
			}
		}
		for _, installation := range selfhost {
			status := "unavailable"
			if installation.Ready {
				status = "ready"
			}
			items = append(items, edgeListItem{ID: installation.NodeID, Name: installation.Name, Source: "self-hosted", Status: status})
		}
		fallback := pool.Mode == "self-hosted-only" && len(selfhost) == 0
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema": "paperboat.edge-list/v1", "observed_at": observedAt, "pool_mode": pool.Mode, "hosted_fallback_display_only": fallback, "edges": items})
		}
		if len(items) == 0 {
			_, err = fmt.Fprintln(command.OutOrStdout(), "No tunnel edges are available to this account.")
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
		if fallback {
			_, err = fmt.Fprintln(command.OutOrStdout(), "Self-hosted-only is enabled with no selected edge; hosted edges are shown for reference but cannot serve your routes until you select mixed mode.")
		}
		return err
	}}
	list.Flags().Bool("json", false, "print JSON")
	root.AddCommand(list)
	return root
}

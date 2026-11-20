package main

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
)

// edgeRegion is one region of the tunnel pool. Paperboat's regions show no servers; the owner's own
// servers are listed by name under their region.
type edgeRegion struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Source string     `json:"source"`
	Status string     `json:"status"`
	Nodes  []edgeNode `json:"nodes,omitempty"`
}

type edgeNode struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// edgeRegions lists Paperboat's regions, then the owner's own selected servers grouped by the region
// they were enrolled in (a region is ready while any of its servers is).
func edgeRegions(hosted []api.HostedEdge, own []api.SelfhostInstallation) []edgeRegion {
	regions := make([]edgeRegion, 0, len(hosted)+len(own))
	for _, edge := range hosted {
		regions = append(regions, edgeRegion{ID: edge.ID, Name: edge.Name, Source: "paperboat", Status: edge.Status})
	}
	index := map[string]int{}
	for _, installation := range own {
		status := "unavailable"
		if installation.Ready {
			status = "ready"
		}
		i, ok := index[installation.Region]
		if !ok {
			i = len(regions)
			index[installation.Region] = i
			regions = append(regions, edgeRegion{ID: installation.Region, Name: installation.Region, Source: "self-hosted", Status: "unavailable"})
		}
		regions[i].Nodes = append(regions[i].Nodes, edgeNode{Name: installation.Name, Status: status})
		if status == "ready" {
			regions[i].Status = "ready"
		}
	}
	return regions
}

func edgeCommand() *cobra.Command {
	root := &cobra.Command{Use: "edge", Short: "Inspect the regions tunnels and previews are served from", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, _ []string) error { return command.Help() }}
	list := &cobra.Command{Use: "list", Short: "List the regions tunnels and previews are served from", Long: `List the regions tunnels and previews are served from for the active
workspace. Managed-only mode lists Paperboat's regions. Self-hosted-only mode
lists only your own servers, one row per server under the region you gave it,
including ones that are currently unavailable. It never falls back to
Paperboat's regions. Self-hosted browser publications require a verified
customer domain and a ready certificate.

Listing a region never guarantees admission for a particular route; domain and
route authorization still apply.

The table identifies each region's name, source and status. A region is ready
while any of its servers is. Use --json for paperboat.edge-list/v1 output with
the active workspace's pool mode and region list.`, Example: "  pb edge list\n  pb edge list --workspace acme\n  pb edge list --json", Args: commandArgs(cobra.NoArgs), RunE: func(command *cobra.Command, args []string) error {
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
		for _, installation := range selfhost {
			if installation.Selected && installation.ScopeKind != "global" {
				selected = append(selected, installation)
			}
		}
		var hosted []api.HostedEdge
		observedAt := time.Now().UTC()
		if pool.Mode == "managed-only" {
			page, err := client.ListHostedEdges(command.Context())
			if err != nil {
				return friendlyCommandError(err)
			}
			observedAt, hosted = page.ObservedAt, page.Items
		}
		regions := edgeRegions(hosted, selected)
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{"schema": "paperboat.edge-list/v1", "observed_at": observedAt, "pool_mode": pool.Mode, "regions": regions})
		}
		if len(regions) == 0 {
			message := "No regions are available in the active workspace."
			if pool.Mode == "self-hosted-only" {
				message = "No selected self-hosted region is visible in the active workspace. Paperboat's regions are not used in self-hosted-only mode."
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), message)
			return err
		}
		writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "REGION\tNAME\tSOURCE\tSTATUS")
		for _, region := range regions {
			if len(region.Nodes) == 0 {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", region.ID, region.Name, region.Source, region.Status)
			}
			for _, node := range region.Nodes {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", region.ID, node.Name, region.Source, node.Status)
			}
		}
		return writer.Flush()
	}}
	list.Flags().Bool("json", false, "print JSON")
	root.AddCommand(list)
	return root
}

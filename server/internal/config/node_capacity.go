package config

import (
	"fmt"
	"strconv"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// NodeCapacity is what the operator panel warns about on the infrastructure capacity page. One place for every
// threshold; each applies per node.
type NodeCapacity struct {
	// MonthlyTrafficBytes is the traffic allowance of one VPS per UTC month. It is compared with the bytes a node counts
	// itself: an edge counts bytes in plus bytes out, so a proxied download is counted in both directions (about twice
	// its payload); a relay counts each forwarded payload byte once. Default 20 TB.
	MonthlyTrafficBytes int64 `json:"monthly_traffic_bytes"`
	// TrafficWarnFraction is the share of the allowance at which a node is flagged (0.8 = 80%).
	TrafficWarnFraction float64 `json:"traffic_warn_fraction"`
	// EdgeCPUWarnPercent flags an edge whose sustained CPU (a five minute average, share of all CPUs) is at or above it.
	EdgeCPUWarnPercent float64 `json:"edge_cpu_warn_percent"`
	// RelayRSSWarnBytes flags a relay whose resident memory is at or above it.
	RelayRSSWarnBytes int64 `json:"relay_rss_warn_bytes"`
	// LoadOutPercent takes a managed node out of rotation when its load (paperboat.node_load_percent) reaches it: an
	// edge leaves the wildcard DNS names, a relay leaves the relay candidate list. LoadInPercent puts it back once its
	// load is below that. Between the two nothing changes.
	LoadOutPercent float64 `json:"load_out_percent"`
	LoadInPercent  float64 `json:"load_in_percent"`
	// EdgeWildcardMaxNodes bounds the nodes in the wildcard DNS answer. The serving set is at most
	// edgeset.ServingNodesPerRegion nodes of each region, so this only bites with more regions than it divides into;
	// the nodes left out are the last of each region's rank, taken round-robin across regions, and the answer also fits
	// tunnelcert.MaxDesiredAddressRecords addresses; a region is never dropped (edge_wildcard_dns.go fitWildcardAnswer).
	EdgeWildcardMaxNodes int `json:"edge_wildcard_max_nodes"`
}

func defaultNodeCapacity() NodeCapacity {
	return NodeCapacity{MonthlyTrafficBytes: 20 * 1_000_000_000_000, TrafficWarnFraction: 0.8, EdgeCPUWarnPercent: 50, RelayRSSWarnBytes: 1 << 30, LoadOutPercent: 85, LoadInPercent: 70, EdgeWildcardMaxNodes: 32}
}

func (n NodeCapacity) validate() error {
	switch {
	case n.MonthlyTrafficBytes < 1_000_000_000:
		return fmt.Errorf("node_capacity.monthly_traffic_bytes must be at least 1 GB")
	case n.TrafficWarnFraction <= 0 || n.TrafficWarnFraction > 1:
		return fmt.Errorf("node_capacity.traffic_warn_fraction must be in (0, 1]")
	case n.EdgeCPUWarnPercent <= 0 || n.EdgeCPUWarnPercent > 100:
		return fmt.Errorf("node_capacity.edge_cpu_warn_percent must be in (0, 100]")
	case n.RelayRSSWarnBytes < 1<<20:
		return fmt.Errorf("node_capacity.relay_rss_warn_bytes must be at least 1 MiB")
	case n.LoadInPercent <= 0 || n.LoadInPercent >= n.LoadOutPercent || n.LoadOutPercent > 100:
		return fmt.Errorf("node_capacity: load_in_percent and load_out_percent must satisfy 0 < in < out <= 100")
	case n.EdgeWildcardMaxNodes < runtimewire.MaximumRegionCarriers:
		return fmt.Errorf("node_capacity.edge_wildcard_max_nodes must be at least %d, the nodes of one workspace's two regions", runtimewire.MaximumRegionCarriers)
	}
	return nil
}

// overlayNodeCapacityEnv reads PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES, the allowance a deployment most often changes, and
// the two load thresholds.
func overlayNodeCapacityEnv(c *Config, lookup func(string) (string, bool)) error {
	if v, ok := lookup("PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES"); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES must be an integer")
		}
		c.NodeCapacity.MonthlyTrafficBytes = n
	}
	for name, target := range map[string]*float64{
		"PAPERBOAT_NODE_CAPACITY_LOAD_OUT_PERCENT": &c.NodeCapacity.LoadOutPercent,
		"PAPERBOAT_NODE_CAPACITY_LOAD_IN_PERCENT":  &c.NodeCapacity.LoadInPercent,
	} {
		if v, ok := lookup(name); ok && v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s must be a number", name)
			}
			*target = n
		}
	}
	return nil
}

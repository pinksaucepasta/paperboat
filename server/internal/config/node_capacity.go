package config

import (
	"fmt"
	"strconv"
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
	// DNSLoadOutPercent takes a managed edge out of the wildcard DNS names when its load (paperboat.node_load_percent)
	// reaches it; DNSLoadInPercent puts it back once its load is below that. Between the two nothing changes.
	DNSLoadOutPercent float64 `json:"dns_load_out_percent"`
	DNSLoadInPercent  float64 `json:"dns_load_in_percent"`
}

func defaultNodeCapacity() NodeCapacity {
	return NodeCapacity{MonthlyTrafficBytes: 20 * 1_000_000_000_000, TrafficWarnFraction: 0.8, EdgeCPUWarnPercent: 50, RelayRSSWarnBytes: 1 << 30, DNSLoadOutPercent: 85, DNSLoadInPercent: 70}
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
	case n.DNSLoadInPercent <= 0 || n.DNSLoadInPercent >= n.DNSLoadOutPercent || n.DNSLoadOutPercent > 100:
		return fmt.Errorf("node_capacity: dns_load_in_percent and dns_load_out_percent must satisfy 0 < in < out <= 100")
	}
	return nil
}

// overlayNodeCapacityEnv reads PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES, the allowance a deployment most often changes, and
// the two wildcard DNS load thresholds.
func overlayNodeCapacityEnv(c *Config, lookup func(string) (string, bool)) error {
	if v, ok := lookup("PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES"); ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES must be an integer")
		}
		c.NodeCapacity.MonthlyTrafficBytes = n
	}
	for name, target := range map[string]*float64{
		"PAPERBOAT_NODE_CAPACITY_DNS_LOAD_OUT_PERCENT": &c.NodeCapacity.DNSLoadOutPercent,
		"PAPERBOAT_NODE_CAPACITY_DNS_LOAD_IN_PERCENT":  &c.NodeCapacity.DNSLoadInPercent,
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

package config

import "testing"

func TestNodeCapacityDefaultsEnvAndBounds(t *testing.T) {
	c := Default()
	if n := c.NodeCapacity; n.MonthlyTrafficBytes != 20_000_000_000_000 || n.TrafficWarnFraction != 0.8 || n.EdgeCPUWarnPercent != 50 || n.RelayRSSWarnBytes != 1<<30 {
		t.Fatalf("defaults = %+v", n)
	}
	if err := c.NodeCapacity.validate(); err != nil {
		t.Fatal(err)
	}
	lookup := func(env map[string]string) func(string) (string, bool) {
		return func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	}
	if err := overlayNodeCapacityEnv(&c, lookup(map[string]string{"PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES": "5000000000000"})); err != nil || c.NodeCapacity.MonthlyTrafficBytes != 5e12 {
		t.Fatalf("env allowance: %v %d", err, c.NodeCapacity.MonthlyTrafficBytes)
	}
	if err := overlayNodeCapacityEnv(&c, lookup(map[string]string{"PAPERBOAT_NODE_MONTHLY_TRAFFIC_BYTES": "20TB"})); err == nil {
		t.Fatal("a non-integer allowance was accepted")
	}
	for name, mutate := range map[string]func(*NodeCapacity){
		"tiny allowance":    func(n *NodeCapacity) { n.MonthlyTrafficBytes = 1 },
		"zero fraction":     func(n *NodeCapacity) { n.TrafficWarnFraction = 0 },
		"fraction over one": func(n *NodeCapacity) { n.TrafficWarnFraction = 1.5 },
		"cpu over 100":      func(n *NodeCapacity) { n.EdgeCPUWarnPercent = 101 },
		"tiny memory":       func(n *NodeCapacity) { n.RelayRSSWarnBytes = 10 },
		"in not below out":  func(n *NodeCapacity) { n.LoadInPercent = n.LoadOutPercent },
		"in zero":           func(n *NodeCapacity) { n.LoadInPercent = 0 },
		"out over 100":      func(n *NodeCapacity) { n.LoadOutPercent = 101 },
	} {
		n := Default().NodeCapacity
		mutate(&n)
		if n.validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := c.NodeCapacity; n.LoadOutPercent != 85 || n.LoadInPercent != 70 {
		t.Fatalf("load defaults %v/%v", n.LoadOutPercent, n.LoadInPercent)
	}
	if err := overlayNodeCapacityEnv(&c, lookup(map[string]string{"PAPERBOAT_NODE_CAPACITY_LOAD_OUT_PERCENT": "90", "PAPERBOAT_NODE_CAPACITY_LOAD_IN_PERCENT": "60"})); err != nil || c.NodeCapacity.LoadOutPercent != 90 || c.NodeCapacity.LoadInPercent != 60 || c.NodeCapacity.validate() != nil {
		t.Fatalf("load env: %v %+v", err, c.NodeCapacity)
	}
	if err := overlayNodeCapacityEnv(&c, lookup(map[string]string{"PAPERBOAT_NODE_CAPACITY_LOAD_IN_PERCENT": "low"})); err == nil {
		t.Fatal("a non-numeric threshold was accepted")
	}
	r := Default().Retention
	if r.NodeCapacityDaily != 90*day {
		t.Fatalf("daily window %v", r.NodeCapacityDaily)
	}
	r.NodeCapacityDaily = 30 * day
	if r.validate(Default().UserMachines.ExpiredAccessSessionRetention) == nil {
		t.Fatal("a window shorter than a month's traffic was accepted")
	}
}

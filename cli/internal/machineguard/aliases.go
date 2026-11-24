//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
)

func numericBrowserLabel(label string) bool {
	port, err := strconv.Atoi(label)
	return err == nil && port > 0 && port <= 65535 && strconv.Itoa(port) == label
}

// A named service may target only one exact numeric registration in the same
// machine and namespace. There are no recursive alias chains.
func browserAliasTarget(aliases map[string]string, name, domain string) string {
	wildcard := strings.HasPrefix(name, "*.")
	parseName := name
	if wildcard {
		parseName = strings.TrimPrefix(name, "*.")
	}
	machine, label, err := splitdns.ParseBrowserHostname(parseName, domain)
	if err != nil || machine == "gateway" {
		return ""
	}
	if !wildcard && label == "" {
		// A top-level name (<name>.<domain>) targets one exact numeric registration of any machine.
		target := aliases[name]
		_, targetLabel, err := splitdns.ParseBrowserHostname(target, domain)
		if err != nil || !numericBrowserLabel(targetLabel) || aliases[target] != splitdns.BrowserGatewayHostname {
			return ""
		}
		return target
	}
	if wildcard && label != "" {
		return ""
	}
	target, explicit := aliases[name]
	if !wildcard && numericBrowserLabel(label) {
		if target == splitdns.BrowserGatewayHostname {
			return name
		}
		return ""
	}
	if !wildcard && !explicit && !splitdns.NumericBrowserLabel(label) {
		target = aliases["*."+machine+"."+domain]
	}
	targetMachine, targetLabel, err := splitdns.ParseBrowserHostname(target, domain)
	if err != nil || targetMachine != machine || !numericBrowserLabel(targetLabel) || aliases[target] != splitdns.BrowserGatewayHostname {
		return ""
	}
	return target
}

// leaseServesName requires the registry lock. Aliases never cross a control
// connection, and cannot remain usable after their protected base lease retires.
func (g *guardServer) leaseServesName(conn controlConn, lease *guardedLease, name string) bool {
	if lease.hostname == name {
		return true
	}
	domain, err := selectedBrowserDomain(g.cfg.StateDir, lease.uid)
	return err == nil && lease.port == 443 && lease.hostname == splitdns.BrowserGatewayHostname && browserAliasTarget(g.aliases[conn], name, domain) != ""
}

func (g *guardServer) replaceAliases(ctx context.Context, conn controlConn, uid string, aliases map[string]string) error {
	if len(aliases) > MaxBrowserAliases {
		return errors.New("too many browser aliases")
	}
	domain, err := selectedBrowserDomain(g.cfg.StateDir, uid)
	if err != nil {
		return err
	}
	leaseNames, gateway := map[string]bool{}, false
	for _, lease := range g.leases {
		if !gatewayRouting || lease.uid == uid {
			leaseNames[lease.hostname] = true
		}
		if lease.owner == conn && lease.hostname == splitdns.BrowserGatewayHostname && lease.ip == splitdns.BrowserGatewayIP && lease.port == 443 {
			gateway = true
		}
	}
	reserved, others := newBrowserNames(domain), newBrowserNames(domain)
	if !gatewayRouting {
		for name, owner := range g.reserved.Names {
			if owner != uid {
				reserved.add(name)
			}
		}
	}
	for other, values := range g.aliases {
		if other != conn && g.sameNamespace(other, uid) {
			for name := range values {
				others.add(name)
			}
		}
	}
	for name := range aliases {
		public := browserAliasTarget(aliases, name, domain) != ""
		if aliases[name] == "" {
			machine, label, err := splitdns.ParseBrowserHostname(name, domain)
			public = err == nil && label != "" && !splitdns.NumericBrowserLabel(label) && browserAliasTarget(aliases, "*."+machine+"."+domain, domain) != ""
		}
		switch owner, held := g.reserved.Names[nameKey(uid, name)]; {
		case !public:
			return errors.New("invalid browser alias")
		case reserved.overlaps(name):
			return errors.New("browser namespace belongs to another OS user")
		case held && owner != uid:
			return errors.New("browser alias belongs to another OS user")
		case leaseNames[name]:
			return errors.New("browser alias conflicts with a machine name")
		case !gateway:
			return errors.New("browser alias requires an owned protected HTTPS listener")
		case others.overlaps(name):
			return errors.New("browser alias belongs to another connection")
		}
	}
	newNames, full := g.aliasReservations(uid, aliases)
	if full {
		g.pruneNames(uid)
		if newNames, full = g.aliasReservations(uid, aliases); full {
			return namesRefusal{reason: "machine guard reservation limit reached for this OS user"}
		}
	}
	if newNames > 0 {
		previousReservations := maps.Clone(g.reserved.Names)
		for name := range aliases {
			g.reserved.Names[nameKey(uid, name)] = uid
		}
		if err := g.persist(); err != nil {
			g.reserved.Names = previousReservations
			return err
		}
	}
	if g.aliases == nil {
		g.aliases = make(map[controlConn]map[string]string)
	}
	g.aliases[conn] = maps.Clone(aliases)
	g.removeUnservedNames(conn)
	return nil
}

// aliasReservations counts the aliases uid does not hold yet, and whether they exceed a reservation limit.
func (g *guardServer) aliasReservations(uid string, aliases map[string]string) (newNames int, full bool) {
	for name := range aliases {
		if !has(g.reserved.Names, nameKey(uid, name)) {
			newNames++
		}
	}
	return newNames, newNames > 0 && (len(g.reserved.Names)+newNames > maxHostNames || g.ownedNames(uid)+newNames > maxUserNames)
}

func (g *guardServer) ownedNames(uid string) int {
	owned := 0
	for _, owner := range g.reserved.Names {
		if owner == uid {
			owned++
		}
	}
	return owned
}

// browserNames indexes names for overlap checks: a fixed wildcard excludes every name of its machine.
type browserNames struct {
	domain         string
	names, parents map[string]bool
}

func newBrowserNames(domain string) browserNames {
	return browserNames{domain: domain, names: map[string]bool{}, parents: map[string]bool{}}
}

// parent is the wildcard that covers name, or "" for a wildcard or foreign name.
func (b browserNames) parent(name string) string {
	machine, _, err := splitdns.ParseBrowserHostname(name, b.domain)
	if err != nil {
		return ""
	}
	return "*." + machine + "." + b.domain
}

func (b browserNames) add(name string) {
	b.names[name] = true
	if parent := b.parent(name); parent != "" {
		b.parents[parent] = true
	}
}

func (b browserNames) overlaps(name string) bool {
	if len(b.names) == 0 {
		return false
	}
	if b.names[name] || b.parents[name] {
		return true
	}
	parent := b.parent(name)
	return parent != "" && b.names[parent]
}

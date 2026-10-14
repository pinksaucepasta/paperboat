//go:build linux || darwin || windows

package machineguard

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

const MaxBrowserAliases = 128

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
	if err != nil || machine == "gateway" || (!wildcard && label == "") {
		return ""
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
	newNames, ownedNames := 0, 0
	for _, owner := range g.reserved.Names {
		if owner == uid {
			ownedNames++
		}
	}
	for name := range aliases {
		public := browserAliasTarget(aliases, name, domain) != ""
		if aliases[name] == "" {
			machine, label, err := splitdns.ParseBrowserHostname(name, domain)
			public = err == nil && label != "" && !splitdns.NumericBrowserLabel(label) && browserAliasTarget(aliases, "*."+machine+"."+domain, domain) != ""
		}
		if !public {
			return errors.New("invalid browser alias")
		}
		for reserved, owner := range g.reserved.Names {
			if owner != uid && browserNamesOverlap(name, reserved, domain) {
				return errors.New("browser namespace belongs to another OS user")
			}
		}
		if owner, ok := g.reserved.Names[name]; ok {
			if owner != uid {
				return errors.New("browser alias belongs to another OS user")
			}
		} else {
			newNames++
		}
		found := false
		for _, lease := range g.leases {
			if lease.hostname == name {
				return errors.New("browser alias conflicts with a machine name")
			}
			if lease.owner == conn && lease.hostname == splitdns.BrowserGatewayHostname && lease.ip == splitdns.BrowserGatewayIP && lease.port == 443 {
				found = true
			}
		}
		if !found {
			return errors.New("browser alias requires an owned protected HTTPS listener")
		}
		for other, values := range g.aliases {
			if other != conn {
				for otherName := range values {
					if browserNamesOverlap(name, otherName, domain) {
						return errors.New("browser alias belongs to another connection")
					}
				}
			}
		}
	}
	if newNames > 0 && (len(g.reserved.Names)+newNames > 4096 || ownedNames+newNames > 512) {
		return errors.New("machine guard reservation limit reached for this OS user")
	}
	previousReservations := maps.Clone(g.reserved.Names)
	for name := range aliases {
		g.reserved.Names[name] = uid
	}
	if newNames > 0 {
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

// Fixed wildcard reservations exclude overlapping names owned by another user.
func browserNamesOverlap(a, b, domain string) bool {
	if a == b {
		return true
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		if !strings.HasPrefix(pair[0], "*.") {
			continue
		}
		machine, _, err := splitdns.ParseBrowserHostname(pair[1], domain)
		if err == nil && pair[0] == "*."+machine+"."+domain {
			return true
		}
	}
	return false
}

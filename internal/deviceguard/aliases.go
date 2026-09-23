//go:build linux || darwin || windows

package deviceguard

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"
)

// leaseServesName requires the registry lock. Aliases never cross a control
// connection, and cannot remain usable after their protected base lease retires.
func (g *guardServer) leaseServesName(conn controlConn, lease *guardedLease, name string) bool {
	return lease.hostname == name || lease.port == 443 && g.aliases[conn][name] == lease.hostname
}

func (g *guardServer) replaceAliases(ctx context.Context, conn controlConn, uid string, aliases map[string]string) error {
	if len(aliases) > 128 {
		return errors.New("too many browser aliases")
	}
	newNames, ownedNames := 0, 0
	for _, owner := range g.reserved.Names {
		if owner == uid {
			ownedNames++
		}
	}
	for name, base := range aliases {
		if !validName(name) || !validName(base) || name == base || strings.Split(name, ".")[1] != strings.Split(base, ".")[1] {
			return errors.New("invalid flat browser alias")
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
				return errors.New("browser alias conflicts with a device name")
			}
			if lease.owner == conn && lease.hostname == base && lease.port == 443 {
				found = true
			}
		}
		if !found {
			return errors.New("browser alias requires an owned protected HTTPS listener")
		}
		for other, values := range g.aliases {
			if other != conn {
				if _, ok := values[name]; ok {
					return errors.New("browser alias belongs to another connection")
				}
			}
		}
	}
	if newNames > 0 && (len(g.reserved.Names)+newNames > 4096 || ownedNames+newNames > 512) {
		return errors.New("device guard reservation limit reached for this OS user")
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
	// Retire certificates and DNS names immediately when their alias disappears.
	for name := range g.certificates[conn] {
		if g.certificateBase(conn, name) == "" {
			delete(g.certificates[conn], name)
		}
	}
	if g.removeUnservedNames(conn) && g.cfg.ConfigureResolver {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := g.configureDomains(cleanup); err != nil {
			g.fail(err)
			return err
		}
	}
	return nil
}

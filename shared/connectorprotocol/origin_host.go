package connectorprotocol

import (
	"net"
	"strings"
)

// OriginHost returns the host part of a route origin address ("host:port", unbracketed IPv6 host).
func OriginHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return strings.Trim(host, "[]")
}

// OriginHostIsLoopback reports whether the origin is on this machine: localhost or a loopback IP.
// Certificate verification guards little on such a connection, so a route that turns it off for any
// other origin is allowed only by explicit choice and shows a warning (CLI and tunnel status).
func OriginHostIsLoopback(address string) bool {
	host := OriginHost(address)
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

// OriginHostIsLocalhostOrIP reports whether the origin host is "localhost" or an IP literal. Neither
// can carry a real certificate name, so an https route to one presents the public hostname as SNI.
func OriginHostIsLocalhostOrIP(address string) bool {
	host := OriginHost(address)
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
}

package selfhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// NodeNames publishes a DNS name for each of Paperboat's own (global) nodes paired by the address pbh detected, so an IPv6-only machine behind
// NAT64 (which can reach an IPv4-only server only by name) reaches it too.
// Prior art: headscale publishes each node's MagicDNS name from its node list (hscontrol/mapper).
type NodeNames struct {
	DB        *db.DB
	Domain    string
	Publisher interface {
		Publish(context.Context, tunnelcert.DesiredAddressPublication) error
		Reconcile(context.Context, tunnelcert.DesiredAddressPublication) error
	}
}

const nodeNameOwner = "selfhost_node:"

// name is the installation key's name: a retried pairing gets the same one.
func (n *NodeNames) name(installationKey string) string {
	sum := sha256.Sum256([]byte(installationKey))
	return "node-" + hex.EncodeToString(sum[:10]) + "." + n.Domain
}

func nodeNamePublication(host string, addresses []string, at time.Time) tunnelcert.DesiredAddressPublication {
	return tunnelcert.DesiredAddressPublication{Owner: nodeNameOwner + strings.SplitN(host, ".", 2)[0], Hostname: host, ResourceGeneration: 1,
		ReadinessVersion: "selfhost-pairing", ObservedAt: at.UTC(), ValidUntil: at.Add(10 * time.Minute), Addresses: addresses, IPv6ReachabilityVerified: true}
}

// Sync publishes every name that is not at the provider as wanted (an address changed, a write failed) and withdraws
// the names of removed nodes. Run by the worker leader.
func (n *NodeNames) Sync(ctx context.Context) error {
	rows, err := n.DB.Pool().Query(ctx, `WITH wanted AS (
 SELECT DISTINCT n.endpoint_host AS host,to_jsonb(i.endpoint_addresses) AS addresses FROM paperboat.selfhost_installations i
 JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id WHERE i.state='active' AND i.endpoint_addresses IS NOT NULL)
SELECT coalesce(w.host,p.hostname),coalesce(w.addresses,'[]'::jsonb) FROM wanted w
FULL JOIN (SELECT * FROM paperboat.edge_dns_publications WHERE owner_id LIKE 'selfhost\_node:%') p ON p.hostname=w.host
WHERE p.hostname IS NULL OR p.state<>'verified' OR p.desired_addresses<>coalesce(w.addresses,'[]'::jsonb)
ORDER BY p.updated_at NULLS FIRST LIMIT 16`)
	if err != nil {
		return err
	}
	defer rows.Close()
	wanted := map[string][]string{}
	for rows.Next() {
		var host string
		var raw []byte
		var addresses []string
		if err = rows.Scan(&host, &raw); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &addresses); err != nil {
			return err
		}
		wanted[host] = addresses
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var errs error
	for host, addresses := range wanted {
		if err = n.Publisher.Reconcile(ctx, nodeNamePublication(host, addresses, time.Now())); err != nil && !tunnelcert.WaitingForDNS(err) {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// nodeName is the name to give this inspected node and its addresses; "" keeps the host it reported.
func (s *Service) nodeName(ctx context.Context, scope Scope, i Inspection, claimAddress, secret string, pin []byte) (string, []string) {
	if s.Names == nil || scope.Kind != "global" || !i.EndpointDetected {
		return "", nil
	}
	dialed, err := netip.ParseAddrPort(claimAddress)
	if err != nil || !publicIngressIP(dialed.Addr()) {
		return "", nil
	}
	addresses := []string{dialed.Addr().Unmap().String()}
	if v6, err := netip.ParseAddr(i.EndpointIPv6); err == nil && v6.Is6() && !v6.Is4In6() && publicIngressIP(v6) && v6 != dialed.Addr() && s.reachesNode(ctx, v6, secret, pin) {
		addresses = append(addresses, v6.String())
	}
	slices.Sort(addresses) // as edge_dns_publications stores them
	return s.Names.name(i.InstallationKey), addresses
}

// reachesNode reports whether the node with this identity answers its claim API at ip.
func (s *Service) reachesNode(ctx context.Context, ip netip.Addr, secret string, pin []byte) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	address := net.JoinHostPort(ip.String(), strconv.Itoa(edgeports.Claim))
	transport := PinnedTransport(pin, address)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var answer Inspection
	nonce := randomToken(32)
	return requestNode(ctx, client, "https://"+address+"/v1/selfhost/inspect", secret, map[string]string{"nonce": nonce}, &answer) == nil && answer.Nonce == nonce
}

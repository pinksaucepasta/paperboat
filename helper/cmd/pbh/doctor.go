package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// doctorDeps are the host and control-plane calls the checks make; tests replace them.
type doctorDeps struct {
	run    commander
	paths  hostPaths
	probe  func(ctx context.Context, transport string, port int, nonce string) (containers.PortProbe, error)
	listen func(nonce string) (port int, stop func(), err error)
	// ingress is the control plane's verdict on the endpoint host, with a reason when it is not verified.
	ingress func(ctx context.Context, host string) (containers.IngressState, error)
	// publicHTTPS is the state of automatic browser HTTPS (nil: not checked).
	publicHTTPS func() (selfhost.PublicHTTPSStatus, error)
}

func doctor(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "installation state directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(out, "doctor", fs)
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected doctor arguments")
	}
	if os.Geteuid() != 0 && *dir == defaultState {
		return elevated(ctx, append([]string{"doctor"}, args...), out)
	}
	c, err := selfhost.ReadConfig(*dir)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("no pbh installation found in "+*dir+"; run pbh install first"), err)
	}
	return runDoctor(ctx, c, doctorDeps{
		run:   execCommander,
		paths: defaultHostPaths(*dir),
		probe: func(ctx context.Context, transport string, port int, nonce string) (containers.PortProbe, error) {
			return containers.ProbePort(ctx, *dir, transport, port, nonce)
		},
		listen: listenTestPort,
		ingress: func(ctx context.Context, host string) (containers.IngressState, error) {
			return containers.CheckIngress(ctx, *dir, host)
		},
		publicHTTPS: func() (selfhost.PublicHTTPSStatus, error) { return selfhost.ReadPublicHTTPSStatus(*dir) },
	}, out)
}

// runDoctor checks what a public TCP edge needs from its host and names the first thing missing.
func runDoctor(ctx context.Context, c selfhost.Config, d doctorDeps, out io.Writer) error {
	tunnel := false
	for _, component := range c.Components {
		tunnel = tunnel || component.Capability == "tunnel"
	}
	problems := 0
	report := func(ok bool, good, bad string) {
		if ok {
			fmt.Fprintln(out, "ok   "+good)
			return
		}
		problems++
		fmt.Fprintln(out, "FIX  "+bad)
	}
	if !tunnel {
		fmt.Fprintln(out, "This installation does not serve tunnels; public TCP ports need no host setup.")
		listeners(ctx, c, d, false, out, report)
		return nil
	}
	current, err := currentOutgoing(d.paths.proc)
	report(err == nil && current == outgoingRange, "outgoing port range is "+outgoingRange+", clear of the public range", fmt.Sprintf("outgoing port range is %q; run pbh install to move it to %s (public TCP ports %d-%d would collide with outgoing connections)", current, outgoingRange, edgeports.PublicTCPMin, edgeports.PublicTCPMax))
	kind := detectFirewall(ctx, d.run)
	rules := hostFirewallRules(c)
	report(kind == "" || firewallRulePresent(ctx, d.run, kind, rules), "host firewall ("+firewallLabel(kind)+") lets "+rulesText(rules)+" in", "the "+kind+" host firewall does not allow "+rulesText(rules)+"; run pbh install as root to add the rule")
	report(edgeGuardPresent(ctx, d.run), "per-address connection limits are on (nftables table "+edgeGuardTable+")", "per-address connection limits are off; install nftables and run pbh install as root")
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	port, stop, err := d.listen(nonce)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("could not open a test port in "+publicRange+" on this host"), err)
	}
	defer stop()
	result, err := d.probe(ctx, "tcp", port, nonce)
	if err != nil {
		report(false, "", "the control plane could not run the reachability check ("+err.Error()+"); check the node is claimed and the service runs, then rerun pbh doctor")
	} else {
		report(result.Reachable, fmt.Sprintf("test port %d is reachable from the control plane", port), reachabilityAdvice(port, result.Detail, rules))
	}
	listeners(ctx, c, d, true, out, report)
	state, err := d.ingress(ctx, c.EndpointHost)
	if err != nil {
		report(false, "", "could not read the endpoint host status from the control plane ("+err.Error()+"); check the node is claimed and the service runs, then rerun pbh doctor")
	} else {
		report(state.Verified, "the control plane verified "+c.EndpointHost+" reaches this node", ingressAdvice(c.EndpointHost, state.Reason))
	}
	if d.publicHTTPS != nil {
		if status, err := d.publicHTTPS(); err == nil && status.Automatic {
			line := status.Describe()
			report(status.LastError == "", line, line+"; the certificate in use is kept until it expires. Check that TCP port 80 of "+c.EndpointHost+" reaches this node and rerun pbh doctor; the service retries every six hours")
		}
	}
	if problems > 0 {
		return commandError(fmt.Sprintf("%d problem(s) found; fix them and rerun pbh doctor", problems))
	}
	return nil
}

// listeners reports every edge and relay listener of c: probed from the control plane when canProbe (the
// probe needs the tunnel component's credential) and the probe accepts the port, otherwise a skip line.
func listeners(ctx context.Context, c selfhost.Config, d doctorDeps, canProbe bool, out io.Writer, report func(ok bool, good, bad string)) {
	for _, component := range c.Components {
		for _, p := range nodePorts(component) {
			name := fmt.Sprintf("%s %d (%s)", strings.ToUpper(p.transport), p.port, p.service)
			if p.skip == "" && !canProbe {
				p.skip = "only an installation with a tunnel component can ask the control plane to probe"
			}
			if p.skip != "" {
				fmt.Fprintf(out, "skip %s: %s; make sure inbound %s %d is allowed\n", name, p.skip, strings.ToUpper(p.transport), p.port)
				continue
			}
			result, err := d.probe(ctx, p.transport, p.port, "")
			if err != nil {
				report(false, "", "the control plane could not check "+name+" ("+err.Error()+"); check the node is claimed and the service runs, then rerun pbh doctor")
				continue
			}
			report(result.Reachable, name+" is reachable from the control plane", nodePortAdvice(name, p.transport, p.port, component.Capability, result.Detail))
		}
	}
}

type nodePort struct {
	transport string
	port      int
	service   string // what listens there, for the report
	skip      string // why the control plane cannot probe it; "" when it can
}

// nodePorts lists every listener of component: its TCP port (probed in edgeports.NodeMin-NodeMax), the
// relay's STUN on UDP at its TCP port (probed only at edgeports.RelayWSS) and its QUIC port (never probed:
// the control plane sends no QUIC). What the probe does not accept comes back with a skip reason.
func nodePorts(component selfhost.Component) []nodePort {
	var ports []nodePort
	if component.TCPPort > 0 {
		p := nodePort{"tcp", component.TCPPort, component.Capability, ""}
		if p.port < edgeports.NodeMin || p.port > edgeports.NodeMax {
			p.skip = fmt.Sprintf("the control plane probes only TCP %d-%d", edgeports.NodeMin, edgeports.NodeMax)
		}
		ports = append(ports, p)
		if component.Capability == "relay" {
			stun := nodePort{"udp", component.TCPPort, "relay STUN", ""}
			if stun.port != edgeports.RelayWSS {
				stun.skip = fmt.Sprintf("the control plane probes STUN only on UDP %d", edgeports.RelayWSS)
			}
			ports = append(ports, stun)
		}
	}
	if component.QUICPort > 0 {
		ports = append(ports, nodePort{"udp", component.QUICPort, component.Capability + " QUIC", "the control plane does not send QUIC"})
	}
	return ports
}

// nodePortAdvice describes what the control plane saw on an edge or relay port and the usual fix.
func nodePortAdvice(name, transport string, port int, capability, detail string) string {
	prefix := name + " is not reachable from the control plane: "
	rule := strings.ToUpper(transport) + " " + strconv.Itoa(port)
	switch detail {
	case "timeout":
		return prefix + "timed out, usually a cloud firewall drop; allow inbound " + rule + " from anywhere (security group, security list or VPC firewall)"
	case "refused":
		return prefix + "connection refused, so the host answered but nothing listens there; run pbh status and check the " + capability + " service"
	case "unreachable":
		return prefix + "rejected by the network (unreachable), usually the host firewall or routing; see the host firewall check above, or allow inbound " + rule + " by hand"
	case "not_public":
		return prefix + "the registered endpoint host does not resolve to a public address; fix --endpoint-host"
	default:
		return prefix + "check result '" + detail + "'; verify the endpoint host and firewalls"
	}
}

func firewallLabel(kind string) string {
	if kind == "" {
		return "none active"
	}
	return kind
}

// reachabilityAdvice turns what the control plane saw into the rule that is missing.
func reachabilityAdvice(port int, detail string, rules []firewallRule) string {
	prefix := fmt.Sprintf("test port %d is not reachable from the control plane: ", port)
	switch detail {
	case "timeout":
		return prefix + "timed out, usually a cloud firewall drop; add " + cloudRule(rules) + " (security group, security list or VPC firewall)"
	case "unreachable":
		return prefix + "rejected by the network (unreachable), usually this host's firewall or routing; run pbh install as root to open " + rulesText(rules) + " (or add the rule by hand)"
	case "refused":
		return prefix + "the host answered but nothing listened; another firewall layer or a port forward is in the way"
	case "nonce_mismatch":
		return prefix + "something else answered on that port; check for a proxy or NAT in front of this host"
	case "not_public":
		return prefix + "the registered endpoint host does not resolve to a public address; fix --endpoint-host"
	default:
		return prefix + "check result '" + detail + "'; verify the endpoint host and firewalls"
	}
}

// ingressAdvice turns what this host sees at the endpoint host into the one fix that is missing.
func ingressAdvice(host, reason string) string {
	prefix := "the control plane has not verified " + host + ": "
	switch reason {
	case "unresolved":
		return prefix + "the name does not resolve; add an A/AAAA record for it that points at this server"
	case "unreachable":
		return prefix + "TCP 443 on it cannot be reached; point its DNS record at this server and open TCP 443 (host firewall and cloud security group)"
	case "identity_mismatch":
		return prefix + "the address answers, but not as this node; its DNS record points at another server, or a proxy or TLS terminator sits in front of port 443"
	default:
		return prefix + "this host reaches itself there, but the control plane has not checked yet; wait a minute, then rerun pbh doctor"
	}
}

// listenTestPort opens a listener on a free port in the public range that answers every connection with
// nonce, so the control plane can tell this host from anything else on the path.
func listenTestPort(nonce string) (int, func(), error) {
	span := big.NewInt(int64(edgeports.PublicTCPMax - edgeports.PublicTCPMin + 1))
	var lastErr error
	for range 100 {
		offset, err := rand.Int(rand.Reader, span)
		if err != nil {
			return 0, nil, err
		}
		port := edgeports.PublicTCPMin + int(offset.Int64())
		listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(port)))
		if err != nil {
			lastErr = err
			continue
		}
		go func() {
			for {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				_ = connection.SetWriteDeadline(time.Now().Add(3 * time.Second))
				_, _ = io.WriteString(connection, nonce)
				_ = connection.Close()
			}
		}()
		return port, func() { _ = listener.Close() }, nil
	}
	return 0, nil, lastErr
}

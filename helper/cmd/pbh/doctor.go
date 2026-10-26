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
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// doctorDeps are the host and control-plane calls the checks make; tests replace them.
type doctorDeps struct {
	run    commander
	paths  hostPaths
	probe  func(ctx context.Context, port int, nonce string) (containers.PortProbe, error)
	listen func(nonce string) (port int, stop func(), err error)
}

func doctor(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "installation state directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.PrintDefaults()
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
		probe: func(ctx context.Context, port int, nonce string) (containers.PortProbe, error) {
			return containers.ProbePort(ctx, *dir, port, nonce)
		},
		listen: listenTestPort,
	}, out)
}

// runDoctor checks what a public TCP edge needs from its host and names the first thing missing.
func runDoctor(ctx context.Context, c selfhost.Config, d doctorDeps, out io.Writer) error {
	tunnel := false
	for _, component := range c.Components {
		tunnel = tunnel || component.Capability == "tunnel"
	}
	if !tunnel {
		fmt.Fprintln(out, "This installation does not serve tunnels; public TCP ports need no host setup.")
		return nil
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
	current, err := currentOutgoing(d.paths.proc)
	report(err == nil && current == outgoingRange, "outgoing port range is "+outgoingRange+", clear of the public range", fmt.Sprintf("outgoing port range is %q; run pbh install to move it to %s (public TCP ports %d-%d would collide with outgoing connections)", current, outgoingRange, edgeports.PublicTCPMin, edgeports.PublicTCPMax))
	kind := detectFirewall(ctx, d.run)
	report(kind == "" || firewallRulePresent(ctx, d.run, kind), "host firewall ("+firewallLabel(kind)+") lets TCP 443 and "+publicRange+" in", "the "+kind+" host firewall does not allow TCP 443 and "+publicRange+"; run pbh install as root to add the rule")
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
	result, err := d.probe(ctx, port, nonce)
	if err != nil {
		report(false, "", "the control plane could not run the reachability check ("+err.Error()+"); check the node is claimed and the service runs, then rerun pbh doctor")
	} else {
		report(result.Reachable, fmt.Sprintf("test port %d is reachable from the control plane", port), reachabilityAdvice(port, result.Detail))
	}
	if problems > 0 {
		return commandError(fmt.Sprintf("%d problem(s) found; fix them and rerun pbh doctor", problems))
	}
	return nil
}

func firewallLabel(kind string) string {
	if kind == "" {
		return "none active"
	}
	return kind
}

// reachabilityAdvice turns what the control plane saw into the rule that is missing.
func reachabilityAdvice(port int, detail string) string {
	prefix := fmt.Sprintf("test port %d is not reachable from the control plane: ", port)
	switch detail {
	case "timeout":
		return prefix + "the connection timed out, so a cloud firewall is dropping it; add " + cloudRule() + " (security group, security list or VPC firewall)"
	case "unreachable":
		return prefix + "this host's firewall rejects it; run pbh install as root to open TCP 443 and " + publicRange + " (or add the rule by hand)"
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

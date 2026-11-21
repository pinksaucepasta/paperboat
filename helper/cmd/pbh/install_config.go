package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// selectedMode is the component set named by --relay, --edge or --both (relay when none is given).
// The stored capability for an edge is still "tunnel".
func selectedMode(fs *flag.FlagSet) (string, error) {
	var chosen []string
	for flagName, mode := range map[string]string{"relay": "relay", "edge": "tunnel", "both": "both"} {
		if fs.Lookup(flagName).Value.String() == "true" {
			chosen = append(chosen, mode)
		}
	}
	switch len(chosen) {
	case 0:
		return "relay", nil
	case 1:
		return chosen[0], nil
	}
	return "", commandError("choose only one of --relay, --edge and --both")
}

func installationMode(c selfhost.Config) string {
	if len(c.Components) == 2 {
		return "both"
	}
	return c.Components[0].Capability
}

// Upgrades retain the installation owner's settings. Explicit conflicting
// settings are refused before images or protected state can be changed.
func existingInstallConfig(dir string, fs *flag.FlagSet) (selfhost.Config, bool, error) {
	c, err := selfhost.ReadConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		return selfhost.Config{}, false, nil
	}
	if err != nil {
		return c, false, err
	}
	expected := map[string]string{"endpoint-host": c.EndpointHost, "name": c.Name, "claim-listen": c.Listen, "preview-domain": c.PreviewDomain, "tunnel-domain": c.TunnelDomain, "runtime-domain": c.RuntimeDomain, "browser-access": strconv.FormatBool(c.BrowserAccess)}
	staged := map[string]struct {
		file   trustFile
		stored string
	}{"https-cert": {publicCertFile, c.PublicHTTPSCertFile}, "https-key": {publicKeyFile, c.PublicHTTPSKeyFile}, "control-ca": {controlCAFile, c.ControlCA}}
	fs.Visit(func(f *flag.Flag) {
		if err != nil {
			return
		}
		if s, ok := staged[f.Name]; ok {
			err = matchStoredTrustFile(dir, s.file, f.Name, f.Value.String(), s.stored)
			return
		}
		if mode, ok := map[string]string{"relay": "relay", "edge": "tunnel", "both": "both"}[f.Name]; ok && f.Value.String() == "true" && mode != installationMode(c) {
			err = commandError(fmt.Sprintf("existing installation is not --%s; omit that flag to preserve installation settings", f.Name))
		}
		if value, ok := expected[f.Name]; ok && value != f.Value.String() {
			err = commandError(fmt.Sprintf("existing --%s differs; omit that flag to preserve installation settings", f.Name))
		}
		if f.Name == "region" || f.Name == "failure-domain" {
			for _, component := range c.Components {
				value := component.Region
				if f.Name == "failure-domain" {
					value = component.FailureDomain
				}
				if value != f.Value.String() {
					err = commandError(fmt.Sprintf("existing --%s differs; omit that flag to preserve installation settings", f.Name))
				}
			}
		}
	})
	return c, true, err
}

func freshInstallConfig(ctx context.Context, fs *flag.FlagSet, mode, region string) (selfhost.Config, error) {
	value := func(name string) string { return fs.Lookup(name).Value.String() }
	host := value("endpoint-host")
	if host == "" {
		var err error
		host, err = publicAddress(ctx)
		if err != nil {
			return selfhost.Config{}, err
		}
	}
	name := value("name")
	if name == "" {
		name = host
	}
	failure := value("failure-domain")
	if failure == "" {
		failure = host
	}
	c := selfhost.Config{Name: name, EndpointHost: host, Listen: value("claim-listen"), PreviewDomain: value("preview-domain"), TunnelDomain: value("tunnel-domain"), RuntimeDomain: value("runtime-domain"), BrowserAccess: value("browser-access") == "true"}
	if mode == "relay" || mode == "both" {
		tcp := 443
		if mode == "both" {
			tcp = edgeports.RelayWSS
		}
		c.Components = append(c.Components, selfhost.Component{Capability: "relay", TCPPort: tcp, QUICPort: edgeports.RelayQUIC, Region: region, FailureDomain: failure, CapacityLimit: 256})
	}
	if mode == "tunnel" || mode == "both" {
		c.Components = append(c.Components, selfhost.Component{Capability: "tunnel", TCPPort: edgeports.CarrierTCP, QUICPort: edgeports.CarrierQUIC, Region: region, FailureDomain: failure, CapacityLimit: 256})
	}
	return c, nil
}

// regionName is the rule the server enforces at enrol (selfhost.validRegion).
var regionName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

const (
	defaultRegion   = "selfhost"
	regionPromptTry = 3
)

// installRegion is the region a fresh install enrols with: --region when given, otherwise the owner's answer on a
// terminal (empty = selfhost, three tries), otherwise selfhost. Servers sharing a region back each other up.
func installRegion(fs *flag.FlagSet, in io.Reader, out io.Writer, terminal bool) (string, error) {
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "region" })
	region := fs.Lookup("region").Value.String()
	if given || !terminal {
		if !regionName.MatchString(region) {
			return "", commandError("--region must start with a lowercase letter and use only lowercase letters, digits, - and _ (at most 63 characters)")
		}
		return region, nil
	}
	r := bufio.NewReader(in)
	for range regionPromptTry {
		fmt.Fprintf(out, "Region name for this server (servers with the same region back each other up) [%s]: ", defaultRegion)
		answer, err := r.ReadString('\n')
		if answer = strings.TrimSpace(answer); answer == "" && err == nil {
			return defaultRegion, nil
		}
		if regionName.MatchString(answer) {
			return answer, nil
		}
		if err != nil {
			break
		}
		fmt.Fprintln(out, "A region starts with a lowercase letter and uses only lowercase letters, digits, - and _ (at most 63 characters).")
	}
	return "", commandError("no valid region given: pass --region <name> (for example --region eu-west)")
}

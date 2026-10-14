package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

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
	expected := map[string]string{"enable": installationMode(c), "endpoint-host": c.EndpointHost, "name": c.Name, "claim-listen": c.Listen, "preview-domain": c.PreviewDomain, "tunnel-domain": c.TunnelDomain, "runtime-domain": c.RuntimeDomain, "https-cert": c.PublicHTTPSCertFile, "https-key": c.PublicHTTPSKeyFile}
	fs.Visit(func(f *flag.Flag) {
		if err != nil {
			return
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

func freshInstallConfig(ctx context.Context, fs *flag.FlagSet) (selfhost.Config, error) {
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
	c := selfhost.Config{Name: name, EndpointHost: host, Listen: value("claim-listen"), PreviewDomain: value("preview-domain"), TunnelDomain: value("tunnel-domain"), RuntimeDomain: value("runtime-domain"), PublicHTTPSCertFile: value("https-cert"), PublicHTTPSKeyFile: value("https-key")}
	mode := value("enable")
	if mode == "relay" || mode == "both" {
		tcp := 443
		if mode == "both" {
			tcp = 27445
		}
		c.Components = append(c.Components, selfhost.Component{Capability: "relay", TCPPort: tcp, QUICPort: 27446, Region: value("region"), FailureDomain: failure, CapacityLimit: 256})
	}
	if mode == "tunnel" || mode == "both" {
		c.Components = append(c.Components, selfhost.Component{Capability: "tunnel", TCPPort: 27443, QUICPort: 27444, Region: value("region"), FailureDomain: failure, CapacityLimit: 256})
	}
	return c, nil
}

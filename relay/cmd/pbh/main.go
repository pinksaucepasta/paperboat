// pbh installs and manages a local, account-agnostic Paperboat data-plane node.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

var version = "development"

const defaultState = "/var/lib/paperboat-selfhost"
const defaultBinaries = "/usr/local/lib/paperboat-selfhost"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pbh:", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use pbh install, pbh selfhost code, pbh status, or pbh uninstall")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(out, "pbh %s\n", version)
		return nil
	case "install":
		return install(ctx, args[1:], out)
	case "selfhost":
		if len(args) < 2 || args[1] != "code" {
			return errors.New("use pbh selfhost code")
		}
		fs := flag.NewFlagSet("selfhost code", flag.ContinueOnError)
		dir := fs.String("state-dir", defaultState, "installation state directory")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if os.Geteuid() != 0 && *dir == defaultState {
			return elevated(ctx, args, out)
		}
		code, address, expires, err := selfhost.CreateCode(*dir)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Claim address: %s\nSelf-host code: %s\nExpires: %s\nPaste the address and code into Dashboard → Network → Add self-hosted node.\n", address, code, expires.UTC().Format(time.RFC3339))
		return nil
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		dir := fs.String("state-dir", defaultState, "installation state directory")
		binaries := fs.String("binary-dir", defaultBinaries, "runtime binaries directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		return selfhost.Run(ctx, *dir, *binaries, out)
	case "status":
		cmd := exec.CommandContext(ctx, "systemctl", "status", "--no-pager", "paperboat-selfhost.service")
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd.Run()
	case "uninstall":
		if len(args) > 1 {
			return errors.New("unexpected arguments")
		}
		if os.Geteuid() != 0 {
			return elevated(ctx, args, out)
		}
		cmd := exec.CommandContext(ctx, "systemctl", "stop", "paperboat-selfhost.service")
		if err := cmd.Run(); err != nil {
			return errors.New("could not stop self-host service; nothing removed")
		}
		_ = exec.CommandContext(ctx, "systemctl", "disable", "paperboat-selfhost.service").Run()
		if err := selfhost.RemoveLocal(defaultState); err != nil {
			return err
		}
		if err := os.Remove("/etc/systemd/system/paperboat-selfhost.service"); err != nil {
			return err
		}
		if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
			return err
		}
		if err := os.RemoveAll(defaultBinaries); err != nil {
			return err
		}
		fmt.Fprintln(out, "Local self-host installation removed. Remove the node from Dashboard → Network to revoke its registry entry.")
		return nil
	default:
		return errors.New("unknown command; use pbh install, pbh selfhost code, pbh status, or pbh uninstall")
	}
}
func elevated(ctx context.Context, args []string, out io.Writer) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sudo", append([]string{binary}, args...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
func install(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	enable := fs.String("enable", "relay", "relay, tunnel, or both")
	host := fs.String("endpoint-host", "", "public IP or DNS name (auto-detected when omitted)")
	name := fs.String("name", "", "installation name")
	region := fs.String("region", "selfhost", "selection region")
	failure := fs.String("failure-domain", "", "independent failure domain (defaults to endpoint host)")
	listen := fs.String("claim-listen", "0.0.0.0:8443", "local claim HTTPS listen address")
	preview := fs.String("preview-domain", "", "preview base domain")
	tunnel := fs.String("tunnel-domain", "", "durable tunnel base domain")
	runtime := fs.String("runtime-domain", "", "runtime base domain")
	dir := fs.String("state-dir", defaultState, "protected installation directory")
	noService := fs.Bool("no-service", false, "initialize without systemd (isolated testing)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected install arguments")
	}
	if *enable != "relay" && *enable != "tunnel" && *enable != "both" {
		return errors.New("--enable must be relay, tunnel, or both")
	}
	if !*noService && os.Geteuid() != 0 {
		return elevated(ctx, append([]string{"install"}, args...), out)
	}
	if *host == "" {
		value, err := publicAddress(ctx)
		if err != nil {
			return err
		}
		*host = value
	}
	if *name == "" {
		*name = *host
	}
	if *failure == "" {
		*failure = *host
	}
	c := selfhost.Config{Name: *name, EndpointHost: *host, Listen: *listen, PreviewDomain: *preview, TunnelDomain: *tunnel, RuntimeDomain: *runtime}
	if *enable == "relay" || *enable == "both" {
		tcp := 443
		if *enable == "both" {
			tcp = 27445
		}
		c.Components = append(c.Components, selfhost.Component{Capability: "relay", TCPPort: tcp, QUICPort: 27446, Region: *region, FailureDomain: *failure, CapacityLimit: 256})
	}
	if *enable == "tunnel" || *enable == "both" {
		c.Components = append(c.Components, selfhost.Component{Capability: "tunnel", TCPPort: 27443, QUICPort: 27444, Region: *region, FailureDomain: *failure, CapacityLimit: 256})
	}
	if err := selfhost.Initialize(*dir, c); err != nil {
		return err
	}
	if !*noService {
		if *dir != defaultState {
			return errors.New("system service requires default state directory")
		}
		if err := installService(ctx); err != nil {
			return err
		}
	}
	ready, err := selfhost.RuntimeReady(*dir)
	if err != nil {
		return err
	}
	if ready {
		fmt.Fprintln(out, "Paperboat self-host service installed; existing ownership and runtime configuration preserved.")
		return nil
	}
	fmt.Fprintln(out, "Paperboat self-host installed. No account or workspace is assigned yet.")
	return run(ctx, []string{"selfhost", "code", "--state-dir", *dir}, out)
}
func publicAddress(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://get.pprbt.dev/selfhost/address", nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("could not detect public address; rerun with --endpoint-host DNS-or-IP")
	}
	defer resp.Body.Close()
	var v struct {
		Address string `json:"address"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v) != nil || net.ParseIP(v.Address) == nil {
		return "", errors.New("could not detect public address; rerun with --endpoint-host DNS-or-IP")
	}
	return v.Address, nil
}
func installService(ctx context.Context) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemd is required for automatic service startup")
	}
	for _, name := range []string{"pbh", "paperboat-relay", "paperboat-tunnel"} {
		path := filepath.Join(defaultBinaries, name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("installed package is missing %s", name)
		}
	}
	service := `[Unit]
Description=Paperboat self-hosted node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/lib/paperboat-selfhost/pbh serve
Restart=on-failure
RestartSec=3
TimeoutStopSec=15
KillMode=control-group
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/paperboat-selfhost

[Install]
WantedBy=multi-user.target
`
	if err := os.WriteFile("/etc/systemd/system/paperboat-selfhost.service", []byte(service), 0644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "paperboat-selfhost.service"}} {
		cmd := exec.CommandContext(ctx, "systemctl", args...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemd %s failed; state preserved for retry", strings.Join(args, " "))
		}
	}
	return nil
}

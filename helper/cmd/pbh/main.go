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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/helper/internal/containers"
	"github.com/pinksaucepasta/paperboat/helper/internal/reporting"
	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

var version = "development"

// Official helper packages receive the configured address endpoint at build time.
var defaultAddressURL string
var defaultRelayImage string
var defaultTunnelImage string

const defaultState = "/var/lib/paperboat-selfhost"
const defaultBinaries = "/usr/local/lib/paperboat-selfhost"

func main() {
	reporter, err := reporting.New("paperboat-selfhost")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx = reporting.WithSupportReference(ctx, reporting.Reference())
	code := execute(ctx, reporter, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	reporter.Close()
	os.Exit(code)
}
func execute(ctx context.Context, reporter *reporting.Reporter, args []string, out, diagnostic io.Writer) (code int) {
	defer func() {
		if recover() != nil {
			fault := reporter.CaptureFailure(ctx, "process_panic", pbhPanic{})
			fmt.Fprintf(diagnostic, "pbh stopped (%s); support reference %s\n", fault.Cause, fault.SupportReference)
			code = 2
		}
	}()
	observe := func(ctx context.Context, definition string, err error) {
		reporter.ObserveFailure(ctx, definition, err)
		reportChildExit(diagnostic, ctx, definition, err)
	}
	if err := runObserved(ctx, args, out, observe); err != nil {
		fault := reporter.CaptureFailure(ctx, "selfhost_command", err)
		message := "operation failed; state retained for retry"
		var owned commandError
		if errors.As(err, &owned) {
			message = owned.Error()
		}
		fmt.Fprintf(diagnostic, "pbh: %s (%s); support reference %s\n", message, fault.Cause, fault.SupportReference)
		return 1
	}
	return 0
}

// reportChildExit writes the stopped runtime container's last output line next
// to its selfhost_child_failed record, which carries only the cause code.
func reportChildExit(w io.Writer, ctx context.Context, definition string, err error) {
	var exit *containers.ChildExit
	if definition != "selfhost_child" || !errors.As(err, &exit) {
		return
	}
	reference := reporting.SupportReference(ctx)
	if reference == "" {
		reference = reporting.Reference()
	}
	fmt.Fprintf(w, "pbh: selfhost_child_failed support reference %s; last output %q\n", reference, exit.LastLine)
}

type pbhPanic struct{}

func (pbhPanic) Error() string { return "panic" }
func (pbhPanic) processPanic() {}

func run(ctx context.Context, args []string, out io.Writer) error {
	return runObserved(ctx, args, out, nil)
}
func runObserved(ctx context.Context, args []string, out io.Writer, observe func(context.Context, string, error)) error {
	if len(args) == 0 {
		return commandError("use pbh install, pbh configure-tls, pbh selfhost code, pbh status, pbh doctor, or pbh uninstall")
	}
	switch args[0] {
	case "check-host":
		if len(args) != 1 {
			return commandError("check-host accepts no arguments")
		}
		if err := containers.CheckHost(); err != nil {
			return fmt.Errorf("%w: %w", commandError("unsupported self-host operating system"), err)
		}
		fmt.Fprintln(out, "Supported self-host operating system and architecture.")
		return nil
	case "version", "--version":
		fmt.Fprintf(out, "pbh %s\n", version)
		return nil
	case "configure-tls":
		return configureTLS(ctx, args[1:], out)
	case "install":
		return install(ctx, args[1:], out)
	case "selfhost":
		if len(args) < 2 || args[1] != "code" {
			return commandError("use pbh selfhost code")
		}
		fs := flag.NewFlagSet("selfhost code", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dir := fs.String("state-dir", defaultState, "installation state directory")
		if err := fs.Parse(args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.SetOutput(out)
				fs.PrintDefaults()
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return commandError("unexpected arguments")
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
		fs.SetOutput(io.Discard)
		dir := fs.String("state-dir", defaultState, "installation state directory")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.SetOutput(out)
				fs.PrintDefaults()
				return nil
			}
			return err
		}
		if fs.NArg() != 0 {
			return commandError("unexpected arguments")
		}
		return serve(ctx, *dir, out, observe)
	case "status":
		cmd := exec.CommandContext(ctx, "systemctl", "status", "--no-pager", "paperboat-selfhost.service")
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd.Run()
	case "doctor":
		return doctor(ctx, args[1:], out)
	case "uninstall":
		return uninstall(ctx, args[1:], out)
	default:
		return commandError("unknown command; use pbh install, pbh configure-tls, pbh selfhost code, pbh status, pbh doctor, or pbh uninstall")
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
func install(ctx context.Context, args []string, out io.Writer) (err error) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	enable := fs.String("enable", "relay", "relay, tunnel, or both")
	fs.String("endpoint-host", "", "public IP or DNS name (auto-detected when omitted)")
	fs.String("name", "", "installation name")
	fs.String("region", "selfhost", "selection region")
	fs.String("failure-domain", "", "independent failure domain (defaults to endpoint host)")
	fs.String("claim-listen", "0.0.0.0:8443", "local claim HTTPS listen address")
	fs.String("preview-domain", "", "preview base domain")
	fs.String("tunnel-domain", "", "durable tunnel base domain")
	fs.String("runtime-domain", "", "runtime base domain")
	fs.String("browser-login-origin", "", "console origin that signs browsers in to private previews and tunnels (enables browser access)")
	fs.String("https-cert", "", "browser-trusted HTTPS certificate file for the endpoint hostname")
	fs.String("https-key", "", "owner-only private key file for browser HTTPS")
	fs.String("control-ca", "", "CA certificate file for a private control plane (default: public roots)")
	dir := fs.String("state-dir", defaultState, "protected installation directory")
	noService := fs.Bool("no-service", false, "initialize without systemd (isolated testing)")
	relayImage := fs.String("relay-image", defaultRelayImage, "configured relay container image")
	tunnelImage := fs.String("tunnel-image", defaultTunnelImage, "configured tunnel container image")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.PrintDefaults()
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return commandError("unexpected install arguments")
	}
	if *enable != "relay" && *enable != "tunnel" && *enable != "both" {
		return commandError("--enable must be relay, tunnel, or both")
	}
	if err := containers.CheckHost(); err != nil {
		return fmt.Errorf("%w: %w", commandError("unsupported self-host operating system"), err)
	}
	if !*noService && os.Geteuid() != 0 {
		return elevated(ctx, append([]string{"install"}, args...), out)
	}
	if !*noService && *dir != defaultState {
		return commandError("system service requires default state directory")
	}
	if !*noService {
		if err := servicePackageReady(filepath.Join(defaultBinaries, "pbh")); err != nil {
			return err
		}
	}
	c, existing, err := existingInstallConfig(*dir, fs)
	if err != nil {
		return err
	}
	if existing {
		*enable = installationMode(c)
	}
	// A first install that fails before its runtime starts leaves no state
	// behind; once the runtime is installed, failures keep the installation.
	fresh := !existing
	defer func() {
		if fresh && err != nil {
			err = errors.Join(err, discardFreshInstall(*dir))
		}
	}()
	images := containers.Images{Relay: *relayImage, Tunnel: *tunnelImage}
	selected := strings.Split(strings.ReplaceAll(*enable, "both", "relay,tunnel"), ",")
	for _, capability := range selected {
		if _, err := images.Image(capability); err != nil {
			return fmt.Errorf("%w: %w", commandError("configure the selected runtime image; use the release installer"), err)
		}
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return commandError("Docker is required; run the self-host installer first")
	}
	if !*noService {
		if err := ensureRuntimeImages(ctx, images, selected, out); err != nil {
			return err
		}
	}
	if !existing {
		c, err = freshInstallConfig(ctx, fs)
		if err != nil {
			return err
		}
		if err := checkClaimListen(c.Listen); err != nil {
			return err
		}
		if c, err = stageInstallFiles(*dir, fs, c); err != nil {
			return err
		}
	}
	if err := selfhost.Initialize(*dir, c); err != nil {
		return err
	}
	if err := installRuntimeImages(ctx, *dir, c, images, *noService); err != nil {
		return err
	}
	fresh = false
	if !*noService && slices.Contains(selected, "tunnel") {
		if err := setupHostNetwork(ctx, execCommander, defaultHostPaths(*dir), out); err != nil {
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
	if (*enable == "tunnel" || *enable == "both") && c.PublicHTTPSCertFile == "" {
		fmt.Fprintln(out, "Browser HTTPS requires a certificate trusted by your clients covering --endpoint-host. Configure it with pbh configure-tls --https-cert /absolute/fullchain.pem --https-key /absolute/private-key.pem; the private key needs owner-only permissions.")
	}
	return run(ctx, []string{"selfhost", "code", "--state-dir", *dir}, out)
}

// serve runs the installation. A claim address already in use is reported by
// name, because the service journal is the only place the operator will look.
func serve(ctx context.Context, dir string, out io.Writer, observe func(context.Context, string, error)) error {
	reapplyHostFirewall(ctx, execCommander, defaultHostPaths(dir), out)
	err := containers.Serve(ctx, dir, out, observe)
	if errors.Is(err, syscall.EADDRINUSE) {
		if c, readErr := selfhost.ReadConfig(dir); readErr == nil {
			return claimListenError(c.Listen, err)
		}
	}
	return err
}

func publicAddress(ctx context.Context) (string, error) {
	addressURL := os.Getenv("PAPERBOAT_SELFHOST_ADDRESS_URL")
	if addressURL == "" {
		addressURL = defaultAddressURL
	}
	if addressURL == "" {
		return "", commandError("public address detection is not configured; use --endpoint-host DNS-or-IP")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addressURL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", commandError("could not detect public address; rerun with --endpoint-host DNS-or-IP"), err)
	}
	defer resp.Body.Close()
	var v struct {
		Address string `json:"address"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v) != nil || net.ParseIP(v.Address) == nil {
		return "", commandError("could not detect public address; rerun with --endpoint-host DNS-or-IP")
	}
	return v.Address, nil
}

// servicePackageReady proves the release installer placed the runtime binary
// that the systemd unit executes. Install checks it before writing any state.
func servicePackageReady(binary string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("%w: %w", commandError("systemd is required for automatic service startup"), err)
	}
	return servicePackageBinary(binary)
}

func servicePackageBinary(binary string) error {
	info, err := os.Stat(binary)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError(fmt.Sprintf("the self-host package is missing %s; install with the release installer, which places pbh there, then rerun", binary)), err)
	}
	if !info.Mode().IsRegular() {
		return commandError(fmt.Sprintf("%s is not a regular file; reinstall with the release installer", binary))
	}
	return nil
}

func installService(ctx context.Context) error {
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
	bounded, cancel := context.WithTimeout(ctx, serviceCommandTimeout)
	defer cancel()
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "paperboat-selfhost.service"}, {"restart", "paperboat-selfhost.service"}} {
		cmd := exec.CommandContext(bounded, "systemctl", args...)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %w", commandError("systemd configuration failed; state preserved for retry"), err)
		}
	}
	return nil
}

// commandError text is authored at the command boundary; external error text
// is retained for typed consumers and never reused for public diagnostics.
type commandError string

func (e commandError) Error() string { return string(e) }

func configureTLS(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("configure-tls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("state-dir", defaultState, "protected installation directory")
	cert := fs.String("https-cert", "", "browser-trusted HTTPS certificate file covering the enrolled endpoint hostname")
	key := fs.String("https-key", "", "owner-only private key file for browser HTTPS")
	noService := fs.Bool("no-service", false, "save configuration for a separately managed process; restart it to apply")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.PrintDefaults()
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *cert == "" || *key == "" {
		return commandError("configure-tls requires --https-cert and --https-key")
	}
	if !*noService && *dir != defaultState {
		return commandError("automatic restart requires the default state directory; use --no-service for separately managed processes")
	}
	if !*noService && os.Geteuid() != 0 {
		return elevated(ctx, append([]string{"configure-tls"}, args...), out)
	}
	certFile, err := stageTrustFile(*dir, publicCertFile, *cert)
	if err != nil {
		return err
	}
	keyFile, err := stageTrustFile(*dir, publicKeyFile, *key)
	if err != nil {
		return err
	}
	rollback, err := selfhost.ConfigurePublicHTTPS(*dir, certFile, keyFile)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("browser HTTPS configuration was not changed; check certificate hostname, validity, key pairing, and owner-only key permissions"), err)
	}
	if *noService {
		fmt.Fprintln(out, "Browser HTTPS certificate paths saved. Restart the separately managed pbh process to apply; carrier identity and enrollment are unchanged.")
		return nil
	}
	if err := exec.CommandContext(ctx, "systemctl", "restart", "paperboat-selfhost.service").Run(); err != nil {
		restoreErr := rollback()
		if restoreErr != nil {
			return fmt.Errorf("%w: %w", commandError("service restart failed and certificate configuration could not be restored; repair configuration with pbh configure-tls before restarting"), errors.Join(err, restoreErr))
		}
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if recoveryErr := exec.CommandContext(recoveryCtx, "systemctl", "restart", "paperboat-selfhost.service").Run(); recoveryErr != nil {
			return fmt.Errorf("%w: %w", commandError("service restart failed; previous certificate configuration restored, but service recovery failed; run systemctl restart paperboat-selfhost.service after resolving the service error"), errors.Join(err, recoveryErr))
		}
		return fmt.Errorf("%w: %w", commandError("new certificate configuration was not applied; previous configuration and service restored"), err)
	}
	fmt.Fprintln(out, "Browser HTTPS certificate paths saved and self-host service restarted. Carrier identity and enrollment are unchanged. The certificate owner must renew these files and rerun pbh configure-tls to reload them.")
	return nil
}

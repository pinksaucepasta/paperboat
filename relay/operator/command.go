package operator

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Dispatch handles the operator-local command family before a service parses
// its runtime flags. handled is false when args do not select "operator".
func Dispatch(ctx context.Context, args []string, capability string, stdout io.Writer, client *http.Client) (handled bool, err error) {
	if len(args) == 0 || args[0] != "operator" {
		return false, nil
	}
	if len(args) < 2 {
		return true, errors.New("operator command required: setup, create-code, list, revoke, rotate, or remove")
	}
	command := args[1]
	fs := flag.NewFlagSet("operator "+command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateDir := fs.String("state-dir", "", "absolute persistent operator state directory")
	control := fs.String("control-url", "", "Paperboat control-plane HTTPS URL")
	name := fs.String("name", "", "operator-visible installation name")
	host := fs.String("endpoint-host", "", "public DNS name or IP")
	defaultTCP, defaultQUIC := 443, 8443
	if capability == "tunnel" {
		defaultTCP, defaultQUIC = 27443, 27444
	}
	tcp := fs.Int("tcp-port", defaultTCP, "public TCP port")
	quic := fs.Int("quic-port", defaultQUIC, "public QUIC/UDP port")
	region := fs.String("region", "", "selection region")
	failure := fs.String("failure-domain", "", "independent failure domain")
	capacity := fs.Int64("capacity-limit", 0, "concurrent connection capacity")
	tlsCert := fs.String("tls-cert", "", "absolute trusted TLS certificate path")
	tlsKey := fs.String("tls-key", "", "absolute protected TLS private key path")
	controlCA := fs.String("control-ca", "", "optional absolute private control CA bundle")
	previewDomain := fs.String("preview-domain", "", "public preview base domain")
	tunnelDomain := fs.String("tunnel-domain", "", "public durable tunnel base domain")
	runtimeDomain := fs.String("runtime-domain", "", "public runtime base domain")
	account := fs.String("account-id", "", "paired account ID")
	if err := fs.Parse(args[2:]); err != nil {
		return true, err
	}
	if fs.NArg() != 0 {
		return true, errors.New("unexpected operator command arguments")
	}
	if strings.TrimSpace(*stateDir) == "" {
		return true, errors.New("--state-dir is required")
	}
	if command == "setup" {
		result, e := Register(ctx, *stateDir, Setup{ControlURL: *control, Name: *name, Capability: capability, EndpointHost: *host, TCPPort: *tcp, QUICPort: *quic, Region: *region, FailureDomain: *failure, CapacityLimit: *capacity, TLSCert: *tlsCert, TLSKey: *tlsKey, ControlCA: *controlCA, PreviewDomain: *previewDomain, TunnelDomain: *tunnelDomain, RuntimeDomain: *runtimeDomain}, client)
		if e != nil {
			return true, e
		}
		fmt.Fprintf(stdout, "Installation %s registered as node %s. Protected runtime files written under %s. Start with: %s run --state-dir %s\n", result.InstallationID, result.NodeID, strings.TrimRight(*stateDir, "/"), "paperboat-"+capability, *stateDir)
		return true, nil
	}
	if command != "create-code" && command != "list" && command != "revoke" && command != "rotate" && command != "rotate-usage" && command != "remove" && command != "verify-ingress" {
		return true, fmt.Errorf("unknown operator command %q", command)
	}
	if command == "revoke" && strings.TrimSpace(*account) == "" {
		return true, errors.New("revoke requires --account-id")
	}
	result, e := Admin(ctx, *stateDir, command, *account, client)
	if e != nil {
		return true, e
	}
	switch command {
	case "create-code":
		fmt.Fprintf(stdout, "Pairing code: %s\nExpires at: %s\n", result.Code, strconv.FormatInt(result.ExpiresAt, 10))
	case "list":
		for _, a := range result.Accounts {
			status := "active"
			if a.RevokedAt != nil {
				status = "revoked"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", a.AccountID, a.Capability, status)
		}
	case "rotate":
		fmt.Fprintf(stdout, "Runtime credential rotated and written to %s/runtime.credential.\n", strings.TrimRight(*stateDir, "/"))
	case "rotate-usage":
		fmt.Fprintf(stdout, "Tunnel usage signing key rotated and written to %s/usage.key.\n", strings.TrimRight(*stateDir, "/"))
	case "revoke":
		fmt.Fprintln(stdout, "Account pairing revoked.")
	case "remove":
		fmt.Fprintln(stdout, "Installation removed from the control plane.")
	case "verify-ingress":
		fmt.Fprintln(stdout, "Public ingress verified; readiness will update after the runtime reports healthy.")
	}
	return true, nil
}

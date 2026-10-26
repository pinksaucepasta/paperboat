package main

import (
	"errors"
	"net"
	"path"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func splitTunnelOrigin(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	at := strings.Index(value, "://")
	if at < 1 || at == len(value)-3 {
		return "", "", errors.New("origin must include a scheme and address")
	}
	scheme := value[:at]
	if scheme != "http" && scheme != "https" && scheme != "h2c" && scheme != "unix" && scheme != "tcp" {
		return "", "", errors.New("origin scheme must be http, https, h2c, unix, or tcp")
	}
	address := value[at+3:]
	if address != strings.TrimSpace(address) || len(address) > 512 || strings.ContainsAny(address, "\x00\r\n\t@?#") {
		return "", "", errors.New("origin address is invalid")
	}
	if scheme == "unix" {
		if !strings.HasPrefix(address, "/") || path.Clean(address) != address {
			return "", "", errors.New("Unix origin must be a clean absolute path")
		}
	} else {
		host, portText, splitErr := net.SplitHostPort(address)
		port, portErr := strconv.Atoi(portText)
		if splitErr != nil || !validTunnelCLIOriginHost(host) || portErr != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
			return "", "", errors.New("origin must contain a canonical host and port")
		}
	}
	return scheme, address, nil
}

func validTunnelCLIName(value string, maximum int) bool {
	if len(value) < 1 || len(value) > maximum || value[0] == '.' || value[0] == '_' || value[0] == '-' {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validTunnelCLIHostname(value string) bool {
	if len(value) < 1 || len(value) > 253 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t /?#@:") {
		return false
	}
	value = strings.TrimPrefix(value, "*.")
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if len(part) < 1 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func validTunnelCLIOriginHost(value string) bool {
	if net.ParseIP(value) != nil || strings.EqualFold(value, "localhost") {
		return true
	}
	if len(value) < 1 || len(value) > 253 || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t /?#@:") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if len(part) < 1 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func tunnelRouteOriginFlags(command *cobra.Command, preserveDefault bool) {
	command.Flags().Bool("preserve-host", preserveDefault, "preserve the incoming Host header")
	command.Flags().String("host-header", "", "override the origin Host header")
	command.Flags().Bool("clear-host-header", false, "remove the origin Host override")
	command.Flags().String("tls-verification", "", "origin TLS verification (system, custom_ca, or insecure_development)")
	command.Flags().String("tls-server-name", "", "explicit origin TLS server name")
	command.Flags().Bool("clear-tls-server-name", false, "remove the explicit origin TLS server name")
	command.Flags().String("ca-reference", "", "protected custom CA reference")
	command.Flags().Bool("clear-ca-reference", false, "remove the custom CA reference")
	command.Flags().String("client-credential-reference", "", "protected origin mTLS credential reference")
	command.Flags().Bool("clear-client-credential-reference", false, "remove the origin mTLS credential reference")
}

func tunnelRouteOriginFlagsChanged(command *cobra.Command) bool {
	for _, name := range []string{"preserve-host", "host-header", "clear-host-header", "tls-verification", "tls-server-name", "clear-tls-server-name", "ca-reference", "clear-ca-reference", "client-credential-reference", "clear-client-credential-reference"} {
		if command.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func applyTunnelRouteOriginFlags(command *cobra.Command, origin api.TunnelRouteOrigin) (api.TunnelRouteOrigin, error) {
	if command.Flags().Changed("preserve-host") {
		origin.PreserveHost, _ = command.Flags().GetBool("preserve-host")
	}
	hostHeader, _ := command.Flags().GetString("host-header")
	clearHostHeader, _ := command.Flags().GetBool("clear-host-header")
	if command.Flags().Changed("host-header") && clearHostHeader {
		return api.TunnelRouteOrigin{}, errors.New("--host-header and --clear-host-header cannot be combined")
	}
	if command.Flags().Changed("host-header") {
		if !validTunnelCLIOriginHost(hostHeader) {
			return api.TunnelRouteOrigin{}, errors.New("host-header is invalid")
		}
		origin.HostOverride = &hostHeader
	} else if clearHostHeader {
		origin.HostOverride = nil
	}
	if origin.Scheme != "https" {
		for _, name := range []string{"tls-verification", "tls-server-name", "clear-tls-server-name", "ca-reference", "clear-ca-reference", "client-credential-reference", "clear-client-credential-reference"} {
			if command.Flags().Changed(name) {
				return api.TunnelRouteOrigin{}, errors.New("origin TLS flags require an https:// origin")
			}
		}
		origin.TLS = nil
		return origin, nil
	}
	if origin.TLS == nil {
		origin.TLS = &api.TunnelRouteTLS{Verification: "system"}
	}
	tls := *origin.TLS
	if command.Flags().Changed("tls-verification") {
		tls.Verification, _ = command.Flags().GetString("tls-verification")
		if tls.Verification != "system" && tls.Verification != "custom_ca" && tls.Verification != "insecure_development" {
			return api.TunnelRouteOrigin{}, errors.New("tls-verification must be system, custom_ca, or insecure_development")
		}
	}
	serverName, _ := command.Flags().GetString("tls-server-name")
	clearServerName, _ := command.Flags().GetBool("clear-tls-server-name")
	if command.Flags().Changed("tls-server-name") && clearServerName {
		return api.TunnelRouteOrigin{}, errors.New("--tls-server-name and --clear-tls-server-name cannot be combined")
	}
	if command.Flags().Changed("tls-server-name") {
		if !validTunnelCLIOriginHost(serverName) {
			return api.TunnelRouteOrigin{}, errors.New("tls-server-name is invalid")
		}
		tls.ServerName = &serverName
	} else if clearServerName {
		tls.ServerName = nil
	}
	caReference, _ := command.Flags().GetString("ca-reference")
	clearCA, _ := command.Flags().GetBool("clear-ca-reference")
	if command.Flags().Changed("ca-reference") && clearCA {
		return api.TunnelRouteOrigin{}, errors.New("--ca-reference and --clear-ca-reference cannot be combined")
	}
	if command.Flags().Changed("ca-reference") {
		if strings.TrimSpace(caReference) == "" {
			return api.TunnelRouteOrigin{}, errors.New("ca-reference is invalid")
		}
		tls.CAReference = &caReference
		tls.Verification = "custom_ca"
	} else if clearCA {
		tls.CAReference = nil
		if tls.Verification == "custom_ca" {
			tls.Verification = "system"
		}
	}
	clientReference, _ := command.Flags().GetString("client-credential-reference")
	clearClient, _ := command.Flags().GetBool("clear-client-credential-reference")
	if command.Flags().Changed("client-credential-reference") && clearClient {
		return api.TunnelRouteOrigin{}, errors.New("--client-credential-reference and --clear-client-credential-reference cannot be combined")
	}
	if command.Flags().Changed("client-credential-reference") {
		if strings.TrimSpace(clientReference) == "" {
			return api.TunnelRouteOrigin{}, errors.New("client-credential-reference is invalid")
		}
		tls.ClientCredentialReference = &clientReference
	} else if clearClient {
		tls.ClientCredentialReference = nil
	}
	if tls.Verification != "custom_ca" && tls.CAReference != nil {
		return api.TunnelRouteOrigin{}, errors.New("ca-reference requires custom_ca TLS verification")
	}
	if tls.Verification == "custom_ca" && tls.CAReference == nil {
		return api.TunnelRouteOrigin{}, errors.New("custom_ca TLS verification requires --ca-reference")
	}
	origin.TLS = &tls
	return origin, nil
}

func normalizeTunnelCLIPathPrefix(value string) (string, error) {
	value = strings.TrimSuffix(value, "*")
	if value == "" || !strings.HasPrefix(value, "/") || len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n\x7f*") {
		return "", errors.New("path must be an absolute URL prefix")
	}
	return value, nil
}

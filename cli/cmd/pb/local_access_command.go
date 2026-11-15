package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"github.com/spf13/cobra"
)

type localAccessCommandError struct {
	message string
	cause   error
}

func (e localAccessCommandError) Error() string { return e.message }
func (e localAccessCommandError) Unwrap() error { return e.cause }

func localAccessFailure(message string, cause error) error {
	if cause == nil {
		return nil
	}
	return localAccessCommandError{message: message, cause: cause}
}

func localAccessConfigCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "local-access",
		Short: "Configure the local browser domain",
		Long:  "Configure the DNS domain this computer uses for private browser addresses such as https://3000.homelab.<domain>. Port names and a machine's front desk are stored on the server and set with `pb port name set` and `pb port front-desk`.",
	}
	domain := localAccessDomainCommand()

	show := &cobra.Command{Use: "show", Short: "Show the local browser domain", Long: "Show the browser domain this computer uses for private browser addresses. This is a read-only view: it does not change trust, machine permissions, active routes, port names or running services.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		return writeLocalAccessShow(command, cfg)
	}}
	show.Flags().Bool("json", false, "print JSON")
	command.AddCommand(domain, show)
	return command
}

func localAccessDomainCommand() *cobra.Command {
	domain := &cobra.Command{
		Use:   "domain",
		Short: "Set the local browser domain",
		Long:  "Choose the DNS domain used for this user's local browser URLs. Changing the domain provisions local trust through the normal privileged installation path and updates the daemon before this command reports success.",
	}
	domainSet := &cobra.Command{Use: "set <domain>", Short: "Set and apply the local browser domain", Long: "Set the browser domain for this local installation. Paperboat validates its DNS scope, provisions the matching local trust, updates the running helper, and persists the setting only through the standard apply workflow.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		domain, err := splitdns.NormalizeBrowserDomain(args[0])
		if err != nil {
			return localAccessFailure("the local browser domain is invalid; use a DNS domain you control, without a scheme, port or wildcard", err)
		}
		return applyLocalAccessChange(command, cfg, domain, "domain-set", "")
	}}
	domainSet.Flags().Bool("json", false, "print JSON")
	domainReset := &cobra.Command{Use: "reset", Short: "Restore the default local browser domain", Long: "Restore the domain of your Paperboat server (recorded when Paperboat was installed) and apply the change. The helper and trust state are reconciled before success is printed.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		// The server's own domain is the one the machine guard recorded at install.
		domain := guardDeploymentDomain(command.Context())
		if domain == "" {
			return localAccessFailure("this computer has no local access domain; run pb install again", errors.New("machine guard records no default domain"))
		}
		return applyLocalAccessChange(command, cfg, domain, "domain-reset", fmt.Sprintf("Local browser domain reset to %s.", domain))
	}}
	domainReset.Flags().Bool("json", false, "print JSON")
	domain.AddCommand(domainSet, domainReset)

	return domain
}

func loadLocalAccessConfig(command *cobra.Command) (*config.Config, error) {
	return config.Load(configPathFlag(command))
}

// applyLocalAccessChange applies a new local access domain.
func applyLocalAccessChange(command *cobra.Command, cfg *config.Config, domain, action, message string) error {
	if err := applyLocalAccessSettings(command.Context(), cfg, domain); err != nil {
		return localAccessFailure("local browser settings could not be applied. Inspect them with `pb config local-access show` before retrying.", err)
	}
	if action == "domain-set" {
		message = fmt.Sprintf("Local browser domain set to %s.", domain)
	}
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		if err := writeCLIJSON(command.OutOrStdout(), map[string]any{
			"path": cfg.Path(), "action": action, "applied": true, "domain": localAccessDomain(command.Context()),
		}); err != nil {
			return localAccessFailure("local browser settings were applied, but confirmation output failed", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(command.OutOrStdout(), message); err != nil {
		return localAccessFailure("local browser settings were applied, but confirmation output failed", err)
	}
	return nil
}

// localAccessGuardStatus asks the machine guard, the only owner of the local
// access domain, for its status.
var localAccessGuardStatus = func(ctx context.Context) (machineguard.RuntimeStatus, error) {
	guard, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return machineguard.RuntimeStatus{}, err
	}
	defer guard.Close()
	return guard.Status(ctx)
}

// localAccessDomain is the domain local URLs use for this user; "" means the
// guard is not reachable or records none.
func localAccessDomain(ctx context.Context) string {
	status, err := localAccessGuardStatus(ctx)
	if err != nil {
		return ""
	}
	return status.BrowserDomain
}

// guardDeploymentDomain is the Paperboat server's own local access domain, as
// the machine guard recorded it when it was installed.
func guardDeploymentDomain(ctx context.Context) string {
	status, err := localAccessGuardStatus(ctx)
	if err != nil {
		return ""
	}
	return status.DefaultBrowserDomain
}

func writeLocalAccessShow(command *cobra.Command, cfg *config.Config) error {
	domain := localAccessDomain(command.Context())
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		if err := writeCLIJSON(command.OutOrStdout(), map[string]any{"path": cfg.Path(), "domain": domain}); err != nil {
			return localAccessFailure("local access settings view could not be written", err)
		}
		return nil
	}
	if domain == "" {
		domain = "(not set up; run pb install)"
	}
	if _, err := fmt.Fprintf(command.OutOrStdout(), "domain: %s\n", domain); err != nil {
		return localAccessFailure("local access settings view could not be written", err)
	}
	return nil
}

package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
)

func localAccessConfigCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "local-access",
		Short: "Configure local browser domains, aliases and reverse proxies",
		Long:  "Configure the browser domain and named service URLs used by this local Paperboat client. These settings only select names for machine ports already authorized elsewhere; they do not grant access or publish additional ports.",
	}
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
		next := cloneLocalAccess(cfg.LocalAccess)
		next.Domain = args[0]
		return applyLocalAccessChange(command, cfg, next, "domain-set", "")
	}}
	domainSet.Flags().Bool("json", false, "print JSON")
	domainReset := &cobra.Command{Use: "reset", Short: "Restore the default local browser domain", Long: "Restore local.pprbt.dev as this client's browser domain and apply the change. The helper and trust state are reconciled before success is printed; configured named services remain attached to their existing machine ports.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		next := cloneLocalAccess(cfg.LocalAccess)
		next.Domain = config.DefaultLocalAccessDomain
		return applyLocalAccessChange(command, cfg, next, "domain-reset", fmt.Sprintf("Local browser domain reset to %s.", config.DefaultLocalAccessDomain))
	}}
	domainReset.Flags().Bool("json", false, "print JSON")
	domain.AddCommand(domainSet, domainReset)

	aliases := &cobra.Command{
		Use:   "alias",
		Short: "Manage named browser aliases for authorized machine ports",
		Long:  "Manage optional DNS names for ports that are already authorized on a machine. Alias configuration never creates a port grant; the browser gateway checks the existing authorization when it resolves and serves each request.",
	}
	aliasSet := &cobra.Command{Use: "set <machine-alias> <name> <port>", Short: "Set and apply a service alias", Long: "Map a nonnumeric DNS service name on one machine alias to an already authorized port. Paperboat validates and applies the local mapping before reporting success, without creating or expanding machine access grants.", Args: cobra.ExactArgs(3), RunE: func(command *cobra.Command, args []string) error {
		port, err := strconv.ParseUint(args[2], 10, 16)
		if err != nil || port == 0 {
			return errors.New("port must be between 1 and 65535")
		}
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		next := cloneLocalAccess(cfg.LocalAccess)
		machineAlias := strings.ToLower(strings.TrimSpace(args[0]))
		name := strings.ToLower(strings.TrimSpace(args[1]))
		replaced := false
		for index := range next.ServiceAliases {
			item := &next.ServiceAliases[index]
			if item.MachineAlias == machineAlias && item.Name == name {
				item.Port = uint16(port)
				replaced = true
				break
			}
		}
		if !replaced {
			next.ServiceAliases = append(next.ServiceAliases, config.LocalServiceAlias{MachineAlias: machineAlias, Name: name, Port: uint16(port)})
		}
		return applyLocalAccessChange(command, cfg, next, "alias-set", fmt.Sprintf("Service alias %s.%s configured for port %d; Paperboat publishes it only while that port is authorized.", name, machineAlias, port))
	}}
	aliasSet.Flags().Bool("json", false, "print JSON")
	aliasUnset := &cobra.Command{Use: "unset <machine-alias> <name>", Short: "Remove a service alias", Long: "Remove one named browser mapping for the selected machine. Paperboat withdraws the local name while preserving its machine authorization and other service aliases, then confirms only after applying the updated configuration.", Args: cobra.ExactArgs(2), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		next := cloneLocalAccess(cfg.LocalAccess)
		machineAlias := strings.ToLower(strings.TrimSpace(args[0]))
		name := strings.ToLower(strings.TrimSpace(args[1]))
		filtered := make([]config.LocalServiceAlias, 0, len(next.ServiceAliases))
		for _, item := range next.ServiceAliases {
			if item.MachineAlias == machineAlias && item.Name == name {
				continue
			}
			filtered = append(filtered, item)
		}
		next.ServiceAliases = filtered
		return applyLocalAccessChange(command, cfg, next, "alias-unset", fmt.Sprintf("Service alias %s.%s removed.", name, machineAlias))
	}}
	aliasUnset.Flags().Bool("json", false, "print JSON")
	aliases.AddCommand(aliasSet, aliasUnset)

	show := &cobra.Command{Use: "show", Short: "Show local browser domains and aliases", Long: "Show the effective browser domain and named service mappings stored in the local configuration. This is a read-only view and does not change trust, machine permissions, active routes, or running services.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		return writeLocalAccessShow(command, cfg)
	}}
	show.Flags().Bool("json", false, "print JSON")
	apply := &cobra.Command{Use: "apply", Short: "Apply local browser settings from the config file", Long: "Validate and apply local browser settings after editing the configuration file directly. Domain changes use the ordinary privileged trust workflow; aliases remain bounded to already authorized machine ports, and errors leave success unreported.", Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		return applyLocalAccessChange(command, cfg, cfg.LocalAccess, "apply", "Local browser settings applied.")
	}}
	apply.Flags().Bool("json", false, "print JSON")

	command.AddCommand(domain, aliases, localAccessProxyCommand(), show, apply)
	return command
}

func loadLocalAccessConfig(command *cobra.Command) (*config.Config, error) {
	return config.Load(configPathFlag(command))
}

func cloneLocalAccess(value config.LocalAccessConfig) config.LocalAccessConfig {
	value.ServiceAliases = append([]config.LocalServiceAlias(nil), value.ServiceAliases...)
	value.MachineProxies = append([]config.LocalMachineProxy(nil), value.MachineProxies...)
	return value
}

func applyLocalAccessChange(command *cobra.Command, cfg *config.Config, next config.LocalAccessConfig, action, message string) error {
	candidate := *cfg
	candidate.LocalAccess = cloneLocalAccess(next)
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := applyLocalAccessSettings(command.Context(), cfg, candidate.LocalAccess); err != nil {
		return fmt.Errorf("apply local browser settings: %w", err)
	}
	if action == "domain-set" {
		message = fmt.Sprintf("Local browser domain set to %s.", candidate.LocalAccess.Domain)
	}
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{
			"path": cfg.Path(), "action": action, "applied": true, "local_access": candidate.LocalAccess,
		})
	}
	_, err := fmt.Fprintln(command.OutOrStdout(), message)
	return err
}

func writeLocalAccessShow(command *cobra.Command, cfg *config.Config) error {
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return writeCLIJSON(command.OutOrStdout(), map[string]any{
			"path": cfg.Path(), "local_access": cfg.LocalAccess,
		})
	}
	if _, err := fmt.Fprintf(command.OutOrStdout(), "domain: %s\nservice_aliases:\n", cfg.LocalAccess.Domain); err != nil {
		return err
	}
	if len(cfg.LocalAccess.ServiceAliases) == 0 {
		if _, err := fmt.Fprintln(command.OutOrStdout(), "  (none)"); err != nil {
			return err
		}
	}
	for _, alias := range cfg.LocalAccess.ServiceAliases {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "  %s.%s -> configured port %d\n", alias.Name, alias.MachineAlias, alias.Port); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(command.OutOrStdout(), "machine_proxies:"); err != nil {
		return err
	}
	for _, proxy := range cfg.LocalAccess.MachineProxies {
		if _, err := fmt.Fprintf(command.OutOrStdout(), "  *.%s.%s -> port %d (numeric ports and explicit aliases take priority)\n", proxy.MachineAlias, cfg.LocalAccess.Domain, proxy.Port); err != nil {
			return err
		}
	}
	return nil
}

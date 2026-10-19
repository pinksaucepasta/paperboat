package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func tunnelDomainCommand() *cobra.Command {
	root := tunnelCommand("domain", "Manage tunnel domains", cobra.NoArgs, nil)
	root.AddCommand(domainListCommand(), domainAddCommand(), domainInstructionsCommand())
	for _, a := range []string{"remove", "verify"} {
		root.AddCommand(domainMutationCommand(a))
	}
	return root
}

func domainInstructionsCommand() *cobra.Command {
	command := tunnelCommand("instructions <tunnel> <domain>", "Show authoritative DNS instructions for a tunnel domain", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		tunnelID, err := resolveTunnelSelectorForCommand(ctx, client, args[0])
		if err != nil {
			return err
		}
		domain, err := resolveTunnelDomain(ctx, client, tunnelID, args[1])
		if err != nil {
			return err
		}
		instructions, err := retryTunnelRead(ctx, func() (api.TunnelDNSInstructions, error) {
			return client.TunnelDomainInstructionsV1(ctx, tunnelID, domain.ID)
		})
		if err != nil {
			return err
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return tunnelOutput(command, instructions, "")
		}
		writer := command.OutOrStdout()
		for _, record := range instructions.Records {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%d\n", record.Name, record.Type, record.Value, record.TTL); err != nil {
				return err
			}
		}
		if instructions.PublicTCPPort > 0 {
			if _, err := fmt.Fprintf(writer, "TCP endpoint\t%s:%d\n", strings.TrimPrefix(instructions.Hostname, "*."), instructions.PublicTCPPort); err != nil {
				return err
			}
		}
		if instructions.Note != "" {
			if _, err := fmt.Fprintln(writer, instructions.Note); err != nil {
				return err
			}
		}
		return nil
	})
	tunnelJSONFlag(command)
	return command
}

func domainListCommand() *cobra.Command {
	command := tunnelCommand("list <tunnel>", "List tunnel domains", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		cursor, limit, e := tunnelPageFlags(command)
		if e != nil {
			return e
		}
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelDomainPage, error) {
			return c.ListTunnelDomainsV1(ctx, tunnelID, cursor, limit)
		})
		if e != nil {
			return e
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return tunnelOutput(command, out, "")
		}
		for _, domain := range out.Items {
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\n", domain.ID, domain.Hostname, domain.State, domain.Certificate.State); err != nil {
				return err
			}
		}
		return writeInventoryContinuation(command, args, out.NextCursor, limit)
	})
	tunnelPageFlag(command)
	tunnelJSONFlag(command)
	return command
}

func domainAddCommand() *cobra.Command {
	command := tunnelCommand("add <tunnel> <hostname>", "Add a tunnel domain", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		host := strings.ToLower(args[1])
		route, _ := command.Flags().GetString("route")
		provider, _ := command.Flags().GetString("provider")
		if !validTunnelCLIHostname(host) {
			return errors.New("domain hostname is invalid")
		}
		if provider == "" || len(provider) > 64 || strings.ContainsAny(provider, "\x00\r\n") {
			return errors.New("DNS provider is invalid")
		}
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		resolvedRoute, e := resolveTunnelRoute(ctx, c, tunnelID, route)
		if e != nil {
			return e
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelDomainMutation, error) {
			return c.CreateTunnelDomainV1(ctx, tunnelID, key, api.TunnelDomainInput{Hostname: host, RouteID: resolvedRoute.ID, Provider: provider})
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Added domain %s; status: %s", out.Domain.ID, tunnelOperationHuman(out.Operation)))
	})
	for _, f := range []string{"route"} {
		command.Flags().String(f, "", f)
		_ = command.MarkFlagRequired(f)
	}
	command.Flags().String("provider", "generic", "DNS provider")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func domainMutationCommand(action string) *cobra.Command {
	command := tunnelCommand(action+" <tunnel> <domain>", strings.ToUpper(action[:1])+action[1:]+" a tunnel domain", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		current, e := resolveTunnelDomain(ctx, c, tunnelID, args[1])
		if e != nil {
			return e
		}
		if action == "remove" {
			if e := confirmMutation(command, "tunnel-domain-remove:"+tunnelID+":"+current.ID+":"+current.ETag, fmt.Sprintf("Remove domain binding %s from tunnel %s? User-owned DNS records remain.", current.ID, tunnelID)); e != nil {
				return e
			}
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		apiAction := action
		if apiAction == "remove" {
			apiAction = "delete"
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelDomainMutation, error) {
			return c.MutateTunnelDomainV1(ctx, tunnelID, current.ID, apiAction, current.ETag, key)
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		human := fmt.Sprintf("%s domain %s; status: %s", tunnelActionPastTense(action), out.Domain.ID, tunnelOperationHuman(out.Operation))
		if action == "remove" {
			human = fmt.Sprintf("Removed domain %s; user-owned DNS records are preserved; status: %s", out.Domain.ID, tunnelOperationHuman(out.Operation))
		}
		return tunnelOutput(command, out, human)
	})
	if action == "remove" {
		command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	}
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func resolveTunnelDomain(ctx context.Context, client *api.Client, tunnel, value string) (api.TunnelDomain, error) {
	if ctx == nil || client == nil || strings.TrimSpace(tunnel) == "" || strings.TrimSpace(value) == "" || tunnel != strings.TrimSpace(tunnel) || value != strings.TrimSpace(value) {
		return api.TunnelDomain{}, api.ErrUnsafeTunnelResponse
	}
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	matches := make(map[string]api.TunnelDomain)
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelDomainPage, error) {
			return client.ListTunnelDomainsV1(ctx, tunnel, cursor, 200)
		})
		if err != nil {
			return api.TunnelDomain{}, err
		}
		for _, domain := range page.Items {
			if domain.ID == value || strings.EqualFold(domain.Hostname, value) {
				matches[domain.ID] = domain
				if len(matches) > 1 {
					return api.TunnelDomain{}, &TunnelDomainSelectorAmbiguousError{}
				}
			}
		}
		if page.NextCursor == "" {
			if len(matches) == 1 {
				for _, domain := range matches {
					return domain, nil
				}
			}
			return api.TunnelDomain{}, errors.New("tunnel domain was not found")
		}
		if _, exists := seen[page.NextCursor]; exists {
			return api.TunnelDomain{}, api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return api.TunnelDomain{}, api.ErrUnsafeTunnelResponse
}

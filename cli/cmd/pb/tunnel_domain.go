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
	root := tunnelCommand("domain", "Serve a tunnel at hostnames under your workspace's domains", cobra.NoArgs, nil)
	root.AddCommand(domainListCommand(), domainAddCommand(), domainRemoveCommand())
	return root
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
	command := tunnelCommand("add <tunnel> <hostname>", "Serve a tunnel route at a hostname under a verified workspace domain (pb domain add)", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		host := strings.ToLower(args[1])
		route, _ := command.Flags().GetString("route")
		if !validTunnelCLIHostname(host) {
			return errors.New("domain hostname is invalid")
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
			return c.CreateTunnelDomainV1(ctx, tunnelID, key, api.TunnelDomainInput{Hostname: host, RouteID: resolvedRoute.ID})
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Serving https://%s; certificate: %s; status: %s", out.Domain.Hostname, out.Domain.Certificate.State, tunnelOperationHuman(out.Operation)))
	})
	command.Flags().String("route", "", "route to serve at the hostname")
	_ = command.MarkFlagRequired("route")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func domainRemoveCommand() *cobra.Command {
	command := tunnelCommand("remove <tunnel> <hostname>", "Stop serving a tunnel at a hostname", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
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
		if e := confirmMutation(command, "tunnel-domain-remove:"+tunnelID+":"+current.ID+":"+current.ETag, fmt.Sprintf("Stop serving tunnel %s at %s? The workspace domain and your DNS records stay.", tunnelID, current.Hostname)); e != nil {
			return e
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelDomainMutation, error) {
			return c.DeleteTunnelDomainV1(ctx, tunnelID, current.ID, current.ETag, key)
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Removed %s from tunnel %s; status: %s", current.Hostname, tunnelID, tunnelOperationHuman(out.Operation)))
	})
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
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

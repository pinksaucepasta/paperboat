package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/spf13/cobra"
)

func tunnelRouteCommand() *cobra.Command {
	root := tunnelCommand("route", "Manage tunnel routes", cobra.NoArgs, nil)
	root.AddCommand(routeListCommand(), routeAddCommand(), routeUpdateCommand(), routeRemoveCommand())
	return root
}

func routeListCommand() *cobra.Command {
	command := tunnelCommand("list <tunnel>", "List tunnel routes", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
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
		out, e := retryTunnelRead(ctx, func() (api.TunnelRoutePage, error) {
			return c.ListTunnelRoutesV1(ctx, tunnelID, cursor, limit)
		})
		if e != nil {
			return e
		}
		if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
			return tunnelOutput(command, out, "")
		}
		for _, route := range out.Items {
			match := route.HostMatch.Type
			if route.HostMatch.Hostname != "" {
				match = route.HostMatch.Hostname
			}
			if route.PathPrefix != nil {
				match += *route.PathPrefix
			}
			endpoint := match
			if route.PublicTCPPort != 0 {
				endpoint = net.JoinHostPort(route.HostMatch.Hostname, strconv.Itoa(int(route.PublicTCPPort)))
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\t%s://%s\t%s\n", route.ID, route.Name, route.Protocol, endpoint, route.Origin.Scheme, route.Origin.Address, route.DesiredState); err != nil {
				return err
			}
		}
		return writeInventoryContinuation(command, args, out.NextCursor, limit)
	})
	tunnelPageFlag(command)
	tunnelJSONFlag(command)
	return command
}

func routeAddCommand() *cobra.Command {
	command := tunnelCommand("add <tunnel>", "Add a tunnel route", cobra.ExactArgs(1), func(command *cobra.Command, args []string) error {
		name, _ := command.Flags().GetString("name")
		protocol, _ := command.Flags().GetString("protocol")
		host, _ := command.Flags().GetString("domain")
		origin, _ := command.Flags().GetString("to")
		scheme, address, e := splitTunnelRouteOrigin(origin)
		if e != nil {
			return e
		}
		if !validTunnelCLIName(name, 80) {
			return errors.New("route name is invalid")
		}
		if protocol != "http" && protocol != "tcp" && protocol != "tcp_private" {
			return errors.New("protocol must be http, tcp, or tcp_private")
		}
		pathPrefix, _ := command.Flags().GetString("path")
		var pathValue *string
		if pathPrefix != "" {
			var pathErr error
			pathPrefix, pathErr = normalizeTunnelCLIPathPrefix(pathPrefix)
			if pathErr != nil {
				return pathErr
			}
			pathValue = &pathPrefix
		}
		priority, _ := command.Flags().GetInt32("priority")
		connectTimeout, _ := command.Flags().GetDuration("connect-timeout")
		idleTimeout, _ := command.Flags().GetDuration("idle-timeout")
		maxStreams, _ := command.Flags().GetInt32("max-streams")
		if priority < 0 || priority > 1000000 {
			return errors.New("priority must be between 0 and 1000000")
		}
		if connectTimeout < 100*time.Millisecond || connectTimeout > 120*time.Second {
			return errors.New("connect-timeout must be between 100ms and 2m")
		}
		if idleTimeout < time.Second || idleTimeout > time.Hour {
			return errors.New("idle-timeout must be between 1s and 1h")
		}
		if maxStreams < 1 || maxStreams > 10000 {
			return errors.New("max-streams must be between 1 and 10000")
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		hostMatch := api.TunnelRouteHostMatch{Type: "catch_all"}
		if host != "" {
			if !validTunnelCLIHostname(host) {
				return errors.New("domain is invalid")
			}
			hostMatch = api.TunnelRouteHostMatch{Type: "exact", Hostname: strings.ToLower(host)}
			if strings.HasPrefix(host, "*.") {
				one := 1
				hostMatch = api.TunnelRouteHostMatch{Type: "one_label_wildcard", Hostname: strings.ToLower(host), WildcardLabels: &one}
			}
		}
		if protocol == "tcp_private" && (host != "" || pathValue != nil || scheme != "tcp") {
			return errors.New("tcp_private routes require tcp:// origin without domain or path matching")
		}
		if protocol == "tcp" && (host != "" || pathValue != nil || scheme != "tcp") {
			return errors.New("public tcp routes use the tunnel managed hostname and require tcp:// origin without --domain or --path")
		}
		if protocol == "http" && scheme == "tcp" {
			return errors.New("HTTP routes cannot target a tcp origin")
		}
		preserveHost, _ := command.Flags().GetBool("preserve-host")
		originValue := api.TunnelRouteOrigin{Scheme: scheme, Address: address, PreserveHost: preserveHost}
		if scheme == "https" {
			originValue.TLS = &api.TunnelRouteTLS{Verification: "system"}
		}
		originValue, e = applyTunnelRouteOriginFlags(command, originValue)
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
		// Public routes answer on the tunnel's own hostname unless --domain names
		// another; the edge cannot serve a public catch-all. Private HTTP routes
		// keep catch_all.
		if protocol == "tcp" || protocol == "http" && host == "" {
			tunnel, getErr := c.GetTunnelV1(ctx, tunnelID)
			if getErr != nil {
				return getErr
			}
			if protocol == "tcp" || tunnel.AccessMode == "public" {
				if tunnel.StableEndpoint == "" {
					return fmt.Errorf("public %s routes require a verified customer domain; list routes with `pb tunnel route list %s`, add a domain with `pb tunnel domain add %s HOSTNAME --route ROUTE_ID`, then retry", strings.ToUpper(protocol), tunnelID, tunnelID)
				}
				u, parseErr := url.Parse(tunnel.StableEndpoint)
				if parseErr != nil || u.Hostname() == "" {
					return api.ErrUnsafeTunnelResponse
				}
				hostMatch = api.TunnelRouteHostMatch{Type: "managed_exact", Hostname: u.Hostname()}
			}
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelRouteMutation, error) {
			return c.CreateTunnelRouteV1(ctx, tunnelID, key, api.TunnelRouteInput{Name: name, Protocol: protocol, HostMatch: hostMatch, PathPrefix: pathValue, Origin: originValue, Priority: priority, ConnectTimeoutMS: int32(connectTimeout / time.Millisecond), IdleTimeoutMS: int32(idleTimeout / time.Millisecond), MaxConcurrentStreams: maxStreams})
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Created route %s; status: %s", out.Route.ID, tunnelOperationHuman(out.Operation)))
	})
	for _, flag := range []string{"name", "to"} {
		command.Flags().String(flag, "", flag)
		_ = command.MarkFlagRequired(flag)
	}
	command.Flags().String("domain", "", "exact hostname or one-label wildcard match")
	command.Flags().String("path", "", "HTTP path prefix, optionally ending in *")
	command.Flags().String("protocol", "http", "route protocol (http, tcp, or tcp_private)")
	tunnelRouteOriginFlags(command, true)
	command.Flags().Int32("priority", 0, "route priority")
	command.Flags().Duration("connect-timeout", 10*time.Second, "origin connect timeout")
	command.Flags().Duration("idle-timeout", 5*time.Minute, "stream idle timeout")
	command.Flags().Int32("max-streams", 128, "maximum concurrent streams")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func routeUpdateCommand() *cobra.Command {
	command := tunnelCommand("update <tunnel> <route>", "Update a tunnel route", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		var patch api.TunnelRoutePatch
		changed := false
		originTargetChanged := false
		var requestedOriginScheme, requestedOriginAddress string
		if command.Flags().Changed("name") {
			v, _ := command.Flags().GetString("name")
			if !validTunnelCLIName(v, 80) {
				return errors.New("route name is invalid")
			}
			patch.Name = &v
			changed = true
		}
		if command.Flags().Changed("to") {
			v, _ := command.Flags().GetString("to")
			scheme, address, err := splitTunnelRouteOrigin(v)
			if err != nil {
				return err
			}
			requestedOriginScheme, requestedOriginAddress = scheme, address
			originTargetChanged = true
			changed = true
		}
		if command.Flags().Changed("protocol") {
			v, _ := command.Flags().GetString("protocol")
			if v != "http" && v != "tcp_private" {
				return errors.New("protocol must be http or tcp_private")
			}
			patch.Protocol = &v
			changed = true
		}
		if command.Flags().Changed("domain") {
			v, _ := command.Flags().GetString("domain")
			match := api.TunnelRouteHostMatch{Type: "catch_all"}
			if v != "" {
				if !validTunnelCLIHostname(v) {
					return errors.New("domain is invalid")
				}
				match = api.TunnelRouteHostMatch{Type: "exact", Hostname: strings.ToLower(v)}
				if strings.HasPrefix(v, "*.") {
					one := 1
					match = api.TunnelRouteHostMatch{Type: "one_label_wildcard", Hostname: strings.ToLower(v), WildcardLabels: &one}
				}
			}
			patch.HostMatch = &match
			changed = true
		}
		if command.Flags().Changed("path") {
			v, _ := command.Flags().GetString("path")
			var pathErr error
			v, pathErr = normalizeTunnelCLIPathPrefix(v)
			if pathErr != nil {
				return pathErr
			}
			patch.PathPrefix = &v
			patch.PathPrefixSet = true
			changed = true
		}
		clearPath, _ := command.Flags().GetBool("clear-path")
		if clearPath {
			if command.Flags().Changed("path") {
				return errors.New("--path and --clear-path cannot be combined")
			}
			patch.PathPrefix = nil
			patch.PathPrefixSet = true
			changed = true
		}
		if command.Flags().Changed("priority") {
			v, _ := command.Flags().GetInt32("priority")
			if v < 0 || v > 1000000 {
				return errors.New("priority must be between 0 and 1000000")
			}
			patch.Priority = &v
			changed = true
		}
		if command.Flags().Changed("connect-timeout") {
			v, _ := command.Flags().GetDuration("connect-timeout")
			if v < 100*time.Millisecond || v > 120*time.Second {
				return errors.New("connect-timeout must be between 100ms and 2m")
			}
			ms := int32(v / time.Millisecond)
			patch.ConnectTimeoutMS = &ms
			changed = true
		}
		if command.Flags().Changed("idle-timeout") {
			v, _ := command.Flags().GetDuration("idle-timeout")
			if v < time.Second || v > time.Hour {
				return errors.New("idle-timeout must be between 1s and 1h")
			}
			ms := int32(v / time.Millisecond)
			patch.IdleTimeoutMS = &ms
			changed = true
		}
		if command.Flags().Changed("max-streams") {
			v, _ := command.Flags().GetInt32("max-streams")
			if v < 1 || v > 10000 {
				return errors.New("max-streams must be between 1 and 10000")
			}
			patch.MaxConcurrentStreams = &v
			changed = true
		}
		enable, _ := command.Flags().GetBool("enable")
		disable, _ := command.Flags().GetBool("disable")
		if enable && disable {
			return errors.New("--enable and --disable cannot be combined")
		}
		if enable || disable {
			state := "active"
			if disable {
				state = "disabled"
			}
			patch.DesiredState = &state
			changed = true
		}
		if tunnelRouteOriginFlagsChanged(command) {
			changed = true
		}
		if !changed {
			return errors.New("at least one update flag is required")
		}
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		current, e := resolveTunnelRoute(ctx, c, tunnelID, args[1])
		if e != nil {
			return e
		}
		if originTargetChanged || tunnelRouteOriginFlagsChanged(command) {
			origin := current.Origin
			if originTargetChanged {
				origin.Scheme, origin.Address = requestedOriginScheme, requestedOriginAddress
			}
			origin, e = applyTunnelRouteOriginFlags(command, origin)
			if e != nil {
				return e
			}
			patch.Origin = &origin
		}
		effectiveProtocol, effectiveOrigin, effectiveMatch, effectivePath := current.Protocol, current.Origin, current.HostMatch, current.PathPrefix
		if patch.Protocol != nil {
			effectiveProtocol = *patch.Protocol
		}
		if patch.Origin != nil {
			effectiveOrigin = *patch.Origin
		}
		if patch.HostMatch != nil {
			effectiveMatch = *patch.HostMatch
		}
		if patch.PathPrefix != nil {
			effectivePath = patch.PathPrefix
		} else if patch.PathPrefixSet {
			effectivePath = nil
		}
		if effectiveProtocol == "tcp_private" && (effectiveOrigin.Scheme != "tcp" || effectiveMatch.Type != "catch_all" || effectivePath != nil) {
			return errors.New("tcp_private routes require tcp:// origin without domain or path matching")
		}
		if effectiveProtocol == "http" && effectiveOrigin.Scheme == "tcp" {
			return errors.New("HTTP routes cannot target a tcp origin")
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelRouteMutation, error) {
			return c.PatchTunnelRouteV1(ctx, tunnelID, current.ID, current.ETag, key, patch)
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		if out.Route.Schema == "" {
			out.Route, e = retryTunnelRead(ctx, func() (api.TunnelRoute, error) {
				return c.GetTunnelRouteV1(ctx, tunnelID, current.ID)
			})
			if e != nil {
				return fmt.Errorf("read completed route update: %w", e)
			}
		}
		return tunnelOutput(command, out, fmt.Sprintf("Updated route %s; status: %s", out.Route.ID, tunnelOperationHuman(out.Operation)))
	})
	command.Flags().String("name", "", "new route name")
	command.Flags().String("to", "", "new origin URL")
	command.Flags().String("protocol", "", "new route protocol (http or tcp_private)")
	command.Flags().String("domain", "", "new exact hostname, wildcard, or empty catch-all")
	command.Flags().String("path", "", "new HTTP path prefix")
	command.Flags().Bool("clear-path", false, "remove the HTTP path prefix")
	command.Flags().Int32("priority", 0, "new route priority")
	command.Flags().Duration("connect-timeout", 0, "new origin connect timeout")
	command.Flags().Duration("idle-timeout", 0, "new stream idle timeout")
	command.Flags().Int32("max-streams", 0, "new maximum concurrent streams")
	command.Flags().Bool("enable", false, "enable the route")
	command.Flags().Bool("disable", false, "disable the route")
	tunnelRouteOriginFlags(command, true)
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func routeRemoveCommand() *cobra.Command {
	command := tunnelCommand("remove <tunnel> <route>", "Remove a tunnel route", cobra.ExactArgs(2), func(command *cobra.Command, args []string) error {
		c, ctx, e := tunnelClient(command)
		if e != nil {
			return e
		}
		tunnelID, e := resolveTunnelSelectorForCommand(ctx, c, args[0])
		if e != nil {
			return e
		}
		current, e := resolveTunnelRoute(ctx, c, tunnelID, args[1])
		if e != nil {
			return e
		}
		if e := confirmMutation(command, "tunnel-route-remove:"+tunnelID+":"+current.ID+":"+current.ETag, fmt.Sprintf("Remove route %s from tunnel %s? Matching traffic will stop; the tunnel and domains remain.", current.ID, tunnelID)); e != nil {
			return e
		}
		key, e := tunnelKey()
		if e != nil {
			return e
		}
		out, e := retryTunnelRead(ctx, func() (api.TunnelRouteMutation, error) {
			return c.DeleteTunnelRouteV1(ctx, tunnelID, current.ID, current.ETag, key)
		})
		if e != nil {
			return e
		}
		out.Operation, e = waitForTunnelOperation(command, c, out.Operation)
		if e != nil {
			return e
		}
		return tunnelOutput(command, out, fmt.Sprintf("Removed route %s; the tunnel is preserved; status: %s", out.Route.ID, tunnelOperationHuman(out.Operation)))
	})
	command.Flags().String("confirm", "", "six-character confirmation code from the preview")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

func resolveTunnelRoute(ctx context.Context, client *api.Client, tunnel, value string) (api.TunnelRoute, error) {
	if ctx == nil || client == nil || strings.TrimSpace(tunnel) == "" || strings.TrimSpace(value) == "" || tunnel != strings.TrimSpace(tunnel) || value != strings.TrimSpace(value) {
		return api.TunnelRoute{}, api.ErrUnsafeTunnelResponse
	}
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	matches := make(map[string]api.TunnelRoute)
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelRoutePage, error) {
			return client.ListTunnelRoutesV1(ctx, tunnel, cursor, 200)
		})
		if err != nil {
			return api.TunnelRoute{}, err
		}
		for _, route := range page.Items {
			if route.ID == value || route.Name == value {
				matches[route.ID] = route
				if len(matches) > 1 {
					return api.TunnelRoute{}, &TunnelRouteSelectorAmbiguousError{}
				}
			}
		}
		if page.NextCursor == "" {
			if len(matches) == 1 {
				for _, route := range matches {
					return route, nil
				}
			}
			return api.TunnelRoute{}, errors.New("tunnel route was not found")
		}
		if _, exists := seen[page.NextCursor]; exists {
			return api.TunnelRoute{}, api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return api.TunnelRoute{}, api.ErrUnsafeTunnelResponse
}

package main

import (
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/spf13/cobra"
	"strconv"
	"strings"
)

func localAccessProxyCommand() *cobra.Command {
	command := &cobra.Command{Use: "proxy", Short: "Route machine app names through Coolify or another reverse proxy", Long: "Otherwise-unmapped app names automatically use authorized port 80. Configure a different reverse-proxy port for a machine. Numeric port URLs and explicit aliases keep their destinations. This local setting never grants access; the proxy port must already be authorized."}
	set := &cobra.Command{Use: "set <machine-alias> [port]", Short: "Set the machine reverse-proxy port", Long: "Set the port used by otherwise-unmapped app names on this machine. Port 80 is the automatic default. The selected port must already be authorized; numeric URLs and explicit aliases retain their destinations.", Args: cobra.RangeArgs(1, 2), RunE: func(command *cobra.Command, args []string) error {
		port := uint64(80)
		if len(args) == 2 {
			var err error
			port, err = strconv.ParseUint(args[1], 10, 16)
			if err != nil || port == 0 {
				return errors.New("proxy port must be between 1 and 65535")
			}
		}
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		next := cloneLocalAccess(cfg.LocalAccess)
		machine := strings.ToLower(strings.TrimSpace(args[0]))
		replaced := false
		for index := range next.MachineProxies {
			if next.MachineProxies[index].MachineAlias == machine {
				next.MachineProxies[index].Port = uint16(port)
				replaced = true
				break
			}
		}
		if !replaced {
			next.MachineProxies = append(next.MachineProxies, config.LocalMachineProxy{MachineAlias: machine, Port: uint16(port)})
		}
		return applyLocalAccessChange(command, cfg, next, "proxy-set", fmt.Sprintf("App names under %s.%s configured for proxy port %d. Add matching domains in Coolify or your reverse proxy, then open https://<app>.%s.%s. Numeric ports and explicit aliases keep their destinations. The proxy port must already be authorized.", machine, next.Domain, port, machine, next.Domain))
	}}
	set.Flags().Bool("json", false, "print JSON")
	unset := &cobra.Command{Use: "unset <machine-alias>", Short: "Restore the default reverse-proxy port", Long: "Remove the machine reverse-proxy override and restore automatic app-name routing through authorized port 80. Numeric URLs and explicit aliases retain their destinations. The running gateway is updated before success is reported.", Args: cobra.ExactArgs(1), RunE: func(command *cobra.Command, args []string) error {
		cfg, err := loadLocalAccessConfig(command)
		if err != nil {
			return err
		}
		next := cloneLocalAccess(cfg.LocalAccess)
		machine := strings.ToLower(strings.TrimSpace(args[0]))
		filtered := make([]config.LocalMachineProxy, 0, len(next.MachineProxies))
		for _, proxy := range next.MachineProxies {
			if proxy.MachineAlias != machine {
				filtered = append(filtered, proxy)
			}
		}
		next.MachineProxies = filtered
		return applyLocalAccessChange(command, cfg, next, "proxy-unset", fmt.Sprintf("Default app-name routing restored for %s through authorized port 80. Numeric ports and explicit aliases remain configured.", machine))
	}}
	unset.Flags().Bool("json", false, "print JSON")
	command.AddCommand(set, unset)
	return command
}

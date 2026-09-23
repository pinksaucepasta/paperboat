package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"

	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

type doctorIPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type doctorResolvedDevice struct {
	alias   string
	address netip.Addr
}

var doctorOSResolver doctorIPResolver = net.DefaultResolver
var doctorResolveDevice = func(ctx context.Context, machineID string) (doctorResolvedDevice, error) {
	client, err := daemonrpc.NewClient(ctx, "")
	if err != nil {
		return doctorResolvedDevice{}, err
	}
	defer client.Close()
	device, err := client.ResolveDevice(ctx, machineID)
	if err != nil {
		return doctorResolvedDevice{}, err
	}
	address, err := netip.ParseAddr(device.GetAssignedIp())
	if err != nil {
		return doctorResolvedDevice{}, errors.New("daemon returned an invalid protected device address")
	}
	return doctorResolvedDevice{alias: device.GetAlias(), address: address.Unmap()}, nil
}

func doctorSystemResolutionProbe(snapshot localapi.Snapshot, selected *localapi.MachineStatus) doctorpkg.Probe {
	return doctorpkg.Probe{Code: "system_resolution", Run: func(ctx context.Context) doctorpkg.Check {
		return probeDoctorSystemResolution(ctx, snapshot, selected, doctorResolveDevice, doctorOSResolver)
	}}
}

func probeDoctorSystemResolution(ctx context.Context, snapshot localapi.Snapshot, selected *localapi.MachineStatus, resolveDevice func(context.Context, string) (doctorResolvedDevice, error), resolver doctorIPResolver) doctorpkg.Check {
	base := doctorpkg.Check{Category: "network", Code: "system_resolution"}
	machine, ok := activeDoctorMachine(snapshot.Machines, selected)
	if !ok {
		base.Status = doctorpkg.StatusUnavailable
		base.Summary = "No active Paperboat device is available for a system resolver check."
		return base
	}
	device, err := resolveDevice(ctx, machine.ID)
	if err != nil {
		base.Status = doctorpkg.StatusUnavailable
		base.Summary = "The protected address for an active Paperboat device is unavailable."
		return base
	}
	hostname, err := managedssh.AliasHost(device.alias, snapshot.DeviceSuffix)
	if err != nil || !device.address.IsValid() {
		base.Status = doctorpkg.StatusUnavailable
		base.Summary = "The active Paperboat device does not have a valid managed hostname."
		return base
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			base.Status = doctorpkg.StatusUnavailable
			base.Summary = "The system resolver check did not complete within its deadline."
			return base
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			base.Status = doctorpkg.StatusUnavailable
			base.Summary = "The system resolver check was canceled before it completed."
			return base
		}
		base.Status = doctorpkg.StatusFail
		base.Summary = "The operating system resolver could not resolve an active Paperboat hostname."
		base.Recovery = "Restart the Paperboat local service, check the operating system DNS service, and run pb doctor again."
		return base
	}
	allExpected := len(addresses) > 0
	for _, address := range addresses {
		if address.Unmap() != device.address {
			allExpected = false
			break
		}
	}
	if allExpected {
		base.Status = doctorpkg.StatusPass
		base.Summary = "The operating system resolver maps an active Paperboat hostname to its protected address."
		base.Recovery = "If only a browser fails, check its private-suffix Secure DNS/DNS-over-HTTPS exclusions and certificate trust."
		return base
	}
	base.Status = doctorpkg.StatusFail
	base.Summary = "The operating system resolver returned the wrong address for an active Paperboat hostname."
	base.Recovery = "Restart the Paperboat local service, clear the operating system DNS cache, and run pb doctor again."
	return base
}

func activeDoctorMachine(machines []localapi.MachineStatus, selected *localapi.MachineStatus) (localapi.MachineStatus, bool) {
	active := func(machine localapi.MachineStatus) bool {
		return machine.ID != "" && machine.Eligible && machine.RuntimeState == "ready"
	}
	if selected != nil {
		return *selected, active(*selected)
	}
	candidates := append([]localapi.MachineStatus(nil), machines...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	for _, machine := range candidates {
		if active(machine) {
			return machine, true
		}
	}
	return localapi.MachineStatus{}, false
}

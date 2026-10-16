//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"os"
	"strconv"
	"strings"
)

// The retained carrier owns the account's dispatch address across setup and upgrades.
func prepareRetainedBootstrapListener(stateRoot string, material *bootstrap.Material, resume *bootstrap.ResumeRecord, carrier hostinstall.CarrierConfig) error {
	if material == nil || resume == nil || !material.ReuseIdentity || carrier.RuntimeRoot != stateRoot ||
		carrier.Environment["PAPERBOAT_MACHINE_ID"] != material.UserMachineID ||
		strings.TrimRight(carrier.Environment["PAPERBOAT_CONTROL_URL"], "/") != strings.TrimRight(material.ControlURL, "/") ||
		resume.RuntimeListenAddress != "" && resume.RuntimeListenAddress != carrier.WorkerAddress {
		return bootstrap.ErrResumeBinding
	}
	resume.RuntimeListenAddress = carrier.WorkerAddress
	return prepareBootstrapListener(stateRoot, material, resume)
}

func prepareUnixBootstrapListener(ctx context.Context, stateRoot string, material *bootstrap.Material, resume *bootstrap.ResumeRecord, uid int) error {
	if material == nil || resume == nil {
		return bootstrap.ErrResumeBinding
	}
	if resume.RuntimeListenAddress == "" && material.ReuseIdentity {
		carrier, err := hostinstall.ReadCarrierConfig(ctx, "u"+strconv.Itoa(uid))
		if err == nil {
			return prepareRetainedBootstrapListener(stateRoot, material, resume, carrier)
		}
		if carrier.Instance != "" || !runtimeCausesOnly(err, os.ErrNotExist) {
			return runtimeFailure("verify the installed Paperboat listener before setup", err)
		}
	}
	return prepareBootstrapListener(stateRoot, material, resume)
}

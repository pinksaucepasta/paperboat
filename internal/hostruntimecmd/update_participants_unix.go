//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

type unixUpdateParticipants struct {
	binary   string
	uid, gid int
}

func newUnixUpdateParticipants(binary string, uid, gid int) (updated.UnixParticipants, error) {
	layout, err := service.DefaultLayout(runtime.GOOS)
	if err != nil || binary != layout.Binary {
		return nil, errors.Join(updated.ErrInvalidConfig, err)
	}
	if uid < 0 || gid < 0 {
		return nil, updated.ErrInvalidConfig
	}
	// Initial installation starts updater readiness before committing its
	// metadata. Resolve the protected owner only when an activation probes it.
	return &unixUpdateParticipants{binary: binary, uid: uid, gid: gid}, nil
}

func (p *unixUpdateParticipants) Probe(ctx context.Context) (updated.UnixParticipantProbe, error) {
	var result updated.UnixParticipantProbe
	body, err := p.run(ctx, false)
	if err != nil {
		return result, err
	}
	var probe localdaemon.UpdateProbe
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&probe) != nil || decoder.Decode(&extra) != io.EOF || probe.Schema != localdaemon.UpdateProbeSchema {
		return result, errors.New("invalid enrolled daemon update probe")
	}
	return updated.UnixParticipantProbe{Version: probe.Version, UpdaterVersion: probe.UpdaterVersion, State: probe.State, Machines: probe.Machines, Running: probe.Running, ControlPlaneUnavailableOnly: probe.ControlPlaneUnavailableOnly}, nil
}

func (p *unixUpdateParticipants) Restart(ctx context.Context) error {
	_, err := p.run(ctx, true)
	return err
}

func (p *unixUpdateParticipants) run(ctx context.Context, restart bool) ([]byte, error) {
	if ctx == nil || p == nil || !filepath.IsAbs(p.binary) || p.uid < 0 || p.gid < 0 {
		return nil, updated.ErrInvalidConfig
	}
	owner, err := hostinstall.LoadEnrolledOwner(p.uid)
	if err != nil {
		return nil, err
	}
	if owner.GID != p.gid {
		return nil, updated.ErrInvalidConfig
	}
	arguments := []string{"daemon", "__update-probe"}
	if restart {
		arguments = append(arguments, "--restart")
	}
	command := exec.CommandContext(ctx, p.binary, arguments...)
	command.Env = []string{"HOME=" + owner.Home, "USER=" + owner.User, "LOGNAME=" + owner.User, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
	if runtime.GOOS == "linux" {
		command.Env = append(command.Env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(owner.UID))
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID), Groups: []uint32{uint32(owner.GID)}}}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	var output boundedUpdateProbeOutput
	command.Stdout = &output
	// The fixed child has no user output consumer. Return a bounded typed
	// failure rather than forwarding arbitrary user-service diagnostics.
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return nil, errors.Join(errors.New("enrolled daemon update probe failed"), err, ctx.Err())
	}
	return output.Bytes(), nil
}

type boundedUpdateProbeOutput struct{ buffer bytes.Buffer }

func (b *boundedUpdateProbeOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedUpdateProbeOutput) Write(data []byte) (int, error) {
	if len(data) > (8<<10)-b.buffer.Len() {
		return 0, errors.New("enrolled daemon update probe exceeded output limit")
	}
	return b.buffer.Write(data)
}

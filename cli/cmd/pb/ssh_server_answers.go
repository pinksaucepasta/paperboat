package main

import (
	"bufio"
	"context"
	"errors"

	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/command"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/cli/internal/tunnel"
	"github.com/spf13/cobra"
)

// dialSSHServer opens an SSH stream to the machine and returns it once its sshd has answered; done closes it.
func dialSSHServer(ctx context.Context, commandContext *command.Context, client *api.Client, machine api.UserMachine, target api.ManagedSSHTarget) (tunnel.Conn, *bufio.Reader, func(), error) {
	d, err := buildDeps(commandContext)
	if err != nil {
		return nil, nil, nil, err
	}
	if d.peerApplications == nil {
		return nil, nil, nil, errors.New("Paperboat peer transport is unavailable")
	}
	operationID := newSSHOperationID()
	release := func() {
		releaseOperationCredentials(commandContext.Context, d, machine.ID, operationID, "managed_ssh")
	}
	connection, err := d.peerApplications.DialSSH(ctx, sshConnectInfo(machine, pendingSSHDescriptor(machine, operationID), client.Workspace(), target.OSUser), operationID)
	if err != nil {
		release()
		return nil, nil, nil, commandPreparationFailure{step: prepareSSHTransport, cause: err}
	}
	// The host writes a refusal line before sshd's banner when it knows sshd will refuse (macOS Remote Login), and
	// ends the stream with nothing when it does not serve it; read that first, and stop waiting for a silent
	// stream when the probe's time is up.
	stopWatch := context.AfterFunc(ctx, func() { _ = connection.Close() })
	done := func() {
		stopWatch()
		_ = connection.Close()
		release()
	}
	reader := bufio.NewReader(connection)
	if refused := managedssh.ReadServerStart(reader); refused != nil {
		done()
		return nil, nil, nil, refused
	}
	if _, err := reader.Peek(1); err != nil {
		done()
		return nil, nil, nil, commandPreparationFailure{step: prepareSSHTransport, cause: err}
	}
	return connection, reader, done, nil
}

// sshServerAnswers asks the machine's sshd now: the stored readiness means managed SSH is set up, and stays when sshd
// stops.
func sshServerAnswers(ctx context.Context, c *cobra.Command, machineID string) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	commandContext := actionContext(c, []string{machineID})
	commandContext.Context = ctx
	client, machine, target, err := resolveSSHCommandTarget(commandContext, machineID)
	if err != nil {
		return err
	}
	_, _, done, err := dialSSHServer(ctx, commandContext, client, machine, target)
	if err == nil {
		done()
	}
	return err
}

func sshNotAnswering(err error) string {
	if message := sshFailureMessage(err); message != "" {
		return "SSH does not answer: " + message
	}
	return "SSH does not answer: check that sshd or Remote Login is running there"
}

// refreshSSHReadiness replaces a machine's stored SSH readiness when its sshd does not answer now.
func refreshSSHReadiness(machine *localapi.MachineStatus, answers func(string) error) {
	if machine.SSHReadiness != "ready" {
		return
	}
	if err := answers(machine.ID); err != nil {
		machine.SSHReadiness = "degraded"
		machine.Health = append(machine.Health[:len(machine.Health):len(machine.Health)], localapi.HealthItem{Code: "ssh_server_unreachable", Severity: "warning",
			Title: sshNotAnswering(err), Recovery: "Run pb ssh doctor " + machine.Alias, ETag: "ssh_server_unreachable"})
	}
}

func doctorSSHServerProbe(c *cobra.Command, machine localapi.MachineStatus) doctorpkg.Probe {
	return doctorpkg.Probe{Code: "ssh_server", Run: func(ctx context.Context) doctorpkg.Check {
		check := doctorpkg.Check{Category: "ssh", Code: "ssh_server", Status: doctorpkg.StatusPass, Summary: "The SSH server of " + machine.Alias + " answers."}
		if err := sshServerAnswers(ctx, c, machine.ID); err != nil {
			check.Status, check.Summary = doctorpkg.StatusWarning, sshNotAnswering(err)
			check.Recovery = "Start sshd or Remote Login there, then run `pb ssh doctor " + machine.Alias + "`."
		}
		return check
	}}
}

func ownMachineID() string {
	if _, enrolled := configuredSSHTargetPort(); !enrolled {
		return ""
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return ""
	}
	registration, _ := store.Registration()
	return registration.MachineID
}

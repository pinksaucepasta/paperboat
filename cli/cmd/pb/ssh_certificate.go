package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// sshCertificateRenewal: a connection renews its certificate only when less than this is left; the daemon renews the
// certificates of machines in use long before (renewManagedSSHCertificates), so connections normally ask nothing.
const sshCertificateRenewal = time.Hour

// actionSSHCertificate keeps one machine's managed SSH certificate fresh. OpenSSH runs it from the managed
// fragment's `Match ... exec` before it loads CertificateFile. It always succeeds: a failure is printed and the
// login then fails with pb's explanation, instead of OpenSSH dropping the managed settings of a false Match.
func actionSSHCertificate(cobraCommand *cobra.Command, _ []string) error {
	host, _ := cobraCommand.Flags().GetString("host")
	ctx := actionContext(cobraCommand, nil)
	err := func() error {
		domain, err := configuredManagedSSHDomain(ctx)
		if err != nil {
			return err
		}
		alias, err := managedssh.ParseAliasHost(host, domain)
		if err != nil {
			return err
		}
		client, machine, target, err := resolveSSHCommandTargetWarm(ctx, alias)
		if err != nil {
			return err
		}
		_, err = ensureSSHCertificate(ctx, client, machine, target)
		return err
	}()
	if err != nil {
		fmt.Fprintf(cobraCommand.ErrOrStderr(), "pb: could not get an SSH certificate for %s: %v\n", host, friendlyCommandError(err))
	}
	return nil
}

// ensureSSHCertificate returns this computer's certificate for machine (managedssh.EnsureCertificate) and records
// the use, so the daemon keeps it renewed while the machine is in use.
func ensureSSHCertificate(ctx *command.Context, client *api.Client, machine api.UserMachine, target api.ManagedSSHTarget) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	publicKey, err := os.ReadFile(managedssh.ManagedIdentityPublicKeyPath(home))
	if err != nil {
		return nil, fmt.Errorf("read this computer's managed SSH public key; rerun `pb login`: %w", err)
	}
	issue := func(issueCtx context.Context) (string, error) {
		client, err := sshClientFor(ctx, client)
		if err != nil {
			return "", err
		}
		// Bounded: with the control plane unreachable the connection goes on with the certificate it has.
		issueCtx, cancel := context.WithTimeout(issueCtx, 5*time.Second)
		defer cancel()
		issued, err := client.IssueManagedSSHCertificate(issueCtx, machine.ID, target.MachineGeneration, string(publicKey))
		return issued.Certificate, err
	}
	session, err := currentCLISession(ctx)
	if err != nil {
		return nil, err
	}
	certificate, err := managedssh.EnsureCertificate(ctx.Context, managedssh.CertificateTarget{Home: home, Alias: machine.Alias, OSUser: target.OSUser, Session: session}, publicKey, issue, time.Now(), sshCertificateRenewal)
	if err != nil {
		return nil, err
	}
	return certificate, managedssh.MarkCertificateUsed(home, machine.Alias)
}

// sshCertificateSigner is the managed key presented with its certificate, for pb's own SSH clients.
func sshCertificateSigner(ctx *command.Context, client *api.Client, machine api.UserMachine, target api.ManagedSSHTarget, signer ssh.Signer) (ssh.Signer, error) {
	value, err := ensureSSHCertificate(ctx, client, machine, target)
	if err != nil {
		return nil, err
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey(value)
	if err != nil {
		return nil, err
	}
	certificate, ok := parsed.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("the managed SSH certificate file holds no certificate")
	}
	return ssh.NewCertSigner(certificate, signer)
}

// currentCLISession is this computer's signed-in CLI session (the profile pb login wrote).
func currentCLISession(ctx *command.Context) (string, error) {
	d, err := buildDeps(ctx)
	if err != nil {
		return "", err
	}
	store, err := config.ProfileStoreFor(d.cfg)
	if err != nil {
		return "", err
	}
	profile, err := store.Load(d.cfg.ServerURL)
	if err != nil || profile.CLIClientSessionID == "" {
		return "", errors.Join(errors.New("this computer is not signed in; run `pb login`"), err)
	}
	return profile.CLIClientSessionID, nil
}

package localdaemon

import (
	"context"
	"slices"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

// renewManagedSSHCertificates keeps the certificates of machines a connection used in the last day under an hour
// old (valid for 11 of their 12 hours), so a control-plane outage leaves new logins working for most of the
// certificate validity (F5-08) and connections never wait on a renewal. A failure is retried at the next refresh.
func renewManagedSSHCertificates(ctx context.Context, client *api.Client, home, publicKey, session string, machines []api.UserMachine) {
	used := managedssh.RecentlyUsedCertificates(home, time.Now().Add(-24*time.Hour))
	for _, machine := range machines {
		if machine.SSH == nil || machine.SSH.OSUser == "" || !slices.Contains(used, machine.Alias) {
			continue
		}
		issue := func(issueCtx context.Context) (string, error) {
			issued, err := client.IssueManagedSSHCertificate(issueCtx, machine.ID, uint64(machine.InstallationGeneration), publicKey)
			return issued.Certificate, err
		}
		issueCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = managedssh.EnsureCertificate(issueCtx, managedssh.CertificateTarget{Home: home, Alias: machine.Alias, OSUser: machine.SSH.OSUser, Session: session, MachineID: machine.ID}, []byte(publicKey), issue, time.Now(), 11*time.Hour)
		cancel()
	}
}

// renewingManagedSSHInstall is a refresh that also renews the certificates of recently used machines.
func renewingManagedSSHInstall(install func(context.Context, []api.UserMachine) error, client *api.Client, home, publicKey, session string) func(context.Context, []api.UserMachine) error {
	return func(ctx context.Context, machines []api.UserMachine) error {
		if err := install(ctx, machines); err != nil {
			return err
		}
		renewManagedSSHCertificates(ctx, client, home, publicKey, session, machines)
		return nil
	}
}

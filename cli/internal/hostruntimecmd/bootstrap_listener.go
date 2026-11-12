package hostruntimecmd

import (
	"fmt"
	"net"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
)

// chooseBootstrapLoopbackAddress returns the first candidate nothing listens on.
func chooseBootstrapLoopbackAddress(candidates []string) (string, error) {
	for _, address := range candidates {
		listener, err := net.Listen("tcp4", address)
		if err != nil {
			if !bootstrapAddressInUse(err) {
				return "", fmt.Errorf("check Paperboat runtime listener %s: %w", address, err)
			}
			continue
		}
		if err := listener.Close(); err != nil {
			return "", fmt.Errorf("release Paperboat runtime listener %s: %w", address, err)
		}
		return address, nil
	}
	return "", runtimeFailure(fmt.Sprintf("Paperboat cannot install: all %d local ports it tries (127.0.0.1:%d and the ones after it) are in use. Close the programs using them, then retry.", len(candidates), runtimeport.First))
}

// Checkpoint the locally selected listener before consuming runtime enrollment
// credentials. Server material can be renewed independently of this local choice.
// seed names the OS account: its first candidate port differs from every other user's on the host.
func prepareBootstrapListener(stateRoot string, material *bootstrap.Material, resume *bootstrap.ResumeRecord, seed string) error {
	if material == nil || resume == nil || resume.Material == nil {
		return bootstrap.ErrResumeBinding
	}
	if resume.RuntimeListenAddress == "" {
		address, err := chooseBootstrapLoopbackAddress(runtimeport.Candidates(seed))
		if err != nil {
			return err
		}
		resume.RuntimeListenAddress = address
	}
	material.HelperListenAddress = resume.RuntimeListenAddress
	// Runtime enrollment clears its working credential after consumption. Keep
	// the protected journal independent so later progress saves remain valid.
	journalMaterial := *material
	resume.Material = &journalMaterial
	if err := bootstrap.SaveResume(stateRoot, *resume); err != nil {
		return fmt.Errorf("persist allocated runtime listener: %w", err)
	}
	return nil
}

func rejectFreshBootstrapOverEnrollment(store *identity.Store, resumeErr error) error {
	if store == nil || !runtimeCausesOnly(resumeErr, bootstrap.ErrResumeNotFound) {
		return nil
	}
	registration, err := store.Registration()
	if runtimeCausesOnly(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing machine enrollment: %w", err)
	}
	if registration.MachineID != "" {
		return runtimeFailure("this OS user already has a Paperboat machine enrollment; to complete or repair this machine, sign in to the same account and run `pb setup --name <machine-alias>`; run `pb uninstall` only before enrolling another account")
	}
	return nil
}

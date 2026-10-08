package hostruntimecmd

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtimeport"
)

func chooseBootstrapLoopbackAddress(primary, secondary string) (string, error) {
	for _, address := range []string{primary, secondary} {
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
	return "", fmt.Errorf("Paperboat cannot install: local ports %s and %s are unavailable", primary, secondary)
}

// Checkpoint the locally selected listener before consuming runtime enrollment
// credentials. Server material can be renewed independently of this local choice.
func prepareBootstrapListener(stateRoot string, material *bootstrap.Material, resume *bootstrap.ResumeRecord) error {
	if material == nil || resume == nil || resume.Material == nil {
		return bootstrap.ErrResumeBinding
	}
	if resume.RuntimeListenAddress == "" {
		primary := material.HelperListenAddress
		if primary == "" {
			primary = runtimeport.Primary
		}
		address, err := chooseBootstrapLoopbackAddress(primary, runtimeport.Secondary)
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
	if store == nil || !errors.Is(resumeErr, bootstrap.ErrResumeNotFound) {
		return nil
	}
	registration, err := store.Registration()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing machine enrollment: %w", err)
	}
	if registration.MachineID != "" {
		return errors.New("this OS user already has a Paperboat machine enrollment; to complete or repair this machine, sign in to the same account and run `pb setup --name <machine-alias>`; run `pb uninstall` only before enrolling another account")
	}
	return nil
}

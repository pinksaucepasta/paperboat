package hostruntimecmd

import (
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func allocateBootstrapLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve Paperboat runtime listener: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release Paperboat runtime listener reservation: %w", err)
	}
	return address, nil
}

// Checkpoint the locally selected listener before consuming runtime enrollment
// credentials. Server material can be renewed independently of this local choice.
func prepareBootstrapListener(stateRoot string, material *bootstrap.Material, resume *bootstrap.ResumeRecord) error {
	if material == nil || resume == nil || resume.Material == nil {
		return bootstrap.ErrResumeBinding
	}
	if resume.RuntimeListenAddress == "" {
		address, err := allocateBootstrapLoopbackAddress()
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
		return errors.New("this OS user already has a Paperboat machine enrollment; run `pb uninstall` before enrolling another account")
	}
	return nil
}

//go:build windows

package main

import (
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
)

func freshEnrollmentRuntimePurge(command *cobra.Command) error {
	if code := hostruntimecmd.Execute(command.Context(), []string{"service", "purge"}, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr()); code != 0 {
		return errors.New("runtime purge failed")
	}
	return nil
}

func existingFreshEnrollmentExecutable() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", errors.New("resolve current Windows user")
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		return "", err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(layout.Binary); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if _, err := installsource.Inspect(layout.Binary, "resume", installsource.Custom); err != nil {
		return "", err
	}
	return layout.Binary, nil
}

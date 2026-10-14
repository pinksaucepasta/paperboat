//go:build linux || darwin

package machineguard

import (
	"errors"
	"os"
	"strconv"
)

func validBrowserDomainOwner(owner string) bool {
	uid, err := strconv.ParseUint(owner, 10, 32)
	return err == nil && strconv.FormatUint(uid, 10) == owner
}
func validateBrowserDomainCaller(owner string) error {
	if !validBrowserDomainOwner(owner) {
		return errors.New("invalid browser domain owner UID")
	}
	caller := os.Getenv("SUDO_UID")
	if caller == "" {
		caller = os.Getenv("PAPERBOAT_INVOKING_UID")
	}
	if caller == "" {
		caller = strconv.Itoa(os.Getuid())
	}
	if caller != owner {
		return errors.New("browser domain owner does not match the invoking OS user")
	}
	return nil
}

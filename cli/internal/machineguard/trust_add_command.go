package machineguard

import (
	"fmt"
	"strconv"
)

// darwinTrustAddCommand is the command that adds the Paperboat root to the System keychain. Run from an SSH
// session, `security add-trusted-cert -d` cannot show its approval prompt and is denied at once; run in the
// console user's GUI session (launchctl asuser, as Homebrew and mkcert users do over SSH) the prompt appears
// on the Mac's screen (apple 2026-11-21). With nobody signed in at the screen (consoleUID 0), no prompt can
// appear anywhere, so there is no command to run: ok is false and the caller reports the step as pending.
func darwinTrustAddCommand(consoleUID int, pemPath string) (name string, args []string, ok bool) {
	if consoleUID <= 0 {
		return "", nil, false
	}
	add := []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "ssl", "-k", darwinSystemKeychainPath, pemPath}
	return "/bin/launchctl", append([]string{"asuser", strconv.Itoa(consoleUID)}, add...), true
}

// darwinTrustHeadless names what to do when no one is signed in at the Mac's screen.
func darwinTrustHeadless(pemPath string) error {
	return fmt.Errorf("browser HTTPS trust is pending: macOS asks for approval on the Mac's screen and nobody is signed in there; "+
		"sign in at the Mac and run `sudo security add-trusted-cert -d -r trustRoot -p ssl -k %s %s` in its Terminal", darwinSystemKeychainPath, pemPath)
}

const darwinSystemKeychainPath = "/Library/Keychains/System.keychain"

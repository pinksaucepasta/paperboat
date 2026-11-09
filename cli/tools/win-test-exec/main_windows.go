//go:build windows

// win-test-exec runs a test binary the way a normal user's session would: with a LUA (unelevated) token and a
// private root that the token owns end to end. With -elevated it keeps the runner's own token for the tests in
// elevated_tests.txt, which need Administrators/SYSTEM ownership; the private root is still used. The CI runner is an elevated administrator, so everything it
// creates is owned by BUILTIN\Administrators, which the credential, lock and token-file checks refuse as foreign.
// Use it as `go test -exec win-test-exec.exe ./...`. TEMP, TMP, HOME, USERPROFILE, APPDATA and LOCALAPPDATA point
// into the private root, so no test touches the real profile; the root is removed afterwards.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	disableMaxPrivilege = 0x1
	luaToken            = 0x4
)

var createRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

type tokenOwner struct{ Owner *windows.SID }

func main() {
	args := os.Args[1:]
	elevated := len(args) > 0 && args[0] == "-elevated"
	if elevated {
		args = args[1:]
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: win-test-exec [-elevated] command [args...]")
		os.Exit(2)
	}
	code, err := run(elevated, args[0], args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "win-test-exec:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(elevated bool, name string, args []string) (int, error) {
	var token windows.Token
	if elevated {
		if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
			return 0, fmt.Errorf("open process token: %w", err)
		}
	} else {
		var err error
		if token, err = unelevatedToken(); err != nil {
			return 0, fmt.Errorf("unelevated token: %w", err)
		}
	}
	defer token.Close()
	root, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "pbtest-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(root)
	if err := ownRoot(root, token, !elevated); err != nil {
		return 0, fmt.Errorf("private root %s: %w", root, err)
	}
	if os.Getenv("WIN_TEST_EXEC_DEBUG") != "" {
		debug(root, token)
	}
	command := exec.Command(name, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.Env = privateEnvironment(root)
	if !elevated {
		command.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(token)}
	}
	err = command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}

// unelevatedToken is the LUA token UAC gives a standard session: Administrators becomes deny-only, privileges
// are dropped, integrity is medium. The default owner is the user, so new objects are owned by the user.
func unelevatedToken() (windows.Token, error) {
	var current windows.Token
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_ADJUST_DEFAULT)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &current); err != nil {
		return 0, fmt.Errorf("open process token: %w", err)
	}
	defer current.Close()
	var limited windows.Token
	if r, _, err := createRestrictedToken.Call(uintptr(current), disableMaxPrivilege|luaToken, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&limited))); r == 0 {
		return 0, fmt.Errorf("CreateRestrictedToken: %w", err)
	}
	user, err := limited.GetTokenUser()
	if err != nil {
		limited.Close()
		return 0, fmt.Errorf("token user: %w", err)
	}
	owner := tokenOwner{Owner: user.User.Sid}
	if err := windows.SetTokenInformation(limited, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
		limited.Close()
		return 0, fmt.Errorf("set token owner: %w", err)
	}
	return limited, nil
}

// ownRoot makes the user the owner of root (when setOwner) and grants only the access the token needs; nothing is inherited
// from the runner's profile. The elevated wrapper creates the directory, so the owner is set explicitly.
func ownRoot(root string, token windows.Token, setOwner bool) error {
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	// An elevated run keeps the runner's default owner (Administrators), which its checks expect; the DACL is the
	// same protected one either way, so nothing inherited from the runner's volume (Authenticated Users) applies.
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var owner *windows.SID
	if setOwner {
		info |= windows.OWNER_SECURITY_INFORMATION
		owner = user.User.Sid
	}
	return windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, info, owner, nil, dacl, nil)
}

func privateEnvironment(root string) []string {
	home := filepath.Join(root, "home")
	dirs := map[string]string{
		"TEMP": filepath.Join(root, "tmp"), "TMP": filepath.Join(root, "tmp"),
		"HOME": home, "USERPROFILE": home,
		"APPDATA": filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry[1:], "=") // a leading '=' belongs to the name (=C: drive variables)
		if _, replaced := dirs[strings.ToUpper(entry[:1]+key)]; !replaced {
			env = append(env, entry)
		}
	}
	for key, dir := range dirs {
		os.MkdirAll(dir, 0o700)
		env = append(env, key+"="+dir)
	}
	return env
}

func debug(root string, token windows.Token) {
	user, _ := token.GetTokenUser()
	sd, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	owner := "?"
	if err == nil {
		if o, _, oerr := sd.Owner(); oerr == nil {
			owner = o.String()
		}
	}
	fmt.Fprintf(os.Stderr, "win-test-exec: elevated=%v user=%s root=%s owner=%s sd=%s\n", token.IsElevated(), user.User.Sid, root, owner, sd)
}

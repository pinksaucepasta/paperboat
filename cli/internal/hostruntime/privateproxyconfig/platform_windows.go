//go:build windows

package privateproxyconfig

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var (
	kernel32                     = windows.NewLazySystemDLL("kernel32.dll")
	advapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	wininet                      = windows.NewLazySystemDLL("wininet.dll")
	processIDToSessionID         = kernel32.NewProc("ProcessIdToSessionId")
	wtsGetActiveConsoleSessionID = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	regSetValueExW               = advapi32.NewProc("RegSetValueExW")
	internetSetOptionW           = wininet.NewProc("InternetSetOptionW")
)

type currentUserRegistry struct{}

func (currentUserRegistry) InteractiveUser(context.Context) (bool, error) {
	var processSession uint32
	result, _, callErr := processIDToSessionID.Call(uintptr(os.Getpid()), uintptr(unsafe.Pointer(&processSession)))
	if result == 0 {
		return false, callErr
	}
	activeSession, _, _ := wtsGetActiveConsoleSessionID.Call()
	if uint32(activeSession) == ^uint32(0) || processSession == 0 || processSession != uint32(activeSession) {
		return false, nil
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return false, err
	}
	return !user.User.Sid.Equals(systemSID), nil
}

func (currentUserRegistry) GetAutoConfigURL(context.Context) (valueResult RegistryValue, resultErr error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return RegistryValue{}, err
	}
	defer func() {
		if err := key.Close(); err != nil {
			resultErr = errors.Join(resultErr, err)
			valueResult = RegistryValue{}
		}
	}()
	value, kind, err := key.GetStringValue("AutoConfigURL")
	if errors.Is(err, registry.ErrNotExist) {
		return RegistryValue{}, nil
	}
	if err == nil {
		return RegistryValue{Exists: true, Kind: kind, Data: []byte(value)}, nil
	}
	if !errors.Is(err, registry.ErrUnexpectedType) {
		return RegistryValue{}, err
	}
	length, kind, err := key.GetValue("AutoConfigURL", nil)
	if errors.Is(err, registry.ErrNotExist) {
		return RegistryValue{}, nil
	}
	if err != nil {
		return RegistryValue{}, err
	}
	data := make([]byte, length)
	length, kind, err = key.GetValue("AutoConfigURL", data)
	if err != nil {
		return RegistryValue{}, err
	}
	data = data[:length]
	return RegistryValue{Exists: true, Kind: kind, Data: data}, nil
}

func (currentUserRegistry) SetAutoConfigURL(_ context.Context, value RegistryValue) (resultErr error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, key.Close()) }()
	if !value.Exists {
		err = key.DeleteValue("AutoConfigURL")
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	if value.Kind == registry.SZ || value.Kind == registry.EXPAND_SZ {
		if value.Kind == registry.EXPAND_SZ {
			return key.SetExpandStringValue("AutoConfigURL", string(value.Data))
		}
		return key.SetStringValue("AutoConfigURL", string(value.Data))
	}
	name, err := windows.UTF16PtrFromString("AutoConfigURL")
	if err != nil {
		return err
	}
	var data *byte
	if len(value.Data) > 0 {
		data = &value.Data[0]
	}
	result, _, _ := regSetValueExW.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0, uintptr(value.Kind), uintptr(unsafe.Pointer(data)), uintptr(len(value.Data)))
	if result != 0 {
		return syscall.Errno(result)
	}
	return nil
}

func (currentUserRegistry) BroadcastInternetSettingsChanged(context.Context) error {
	const (
		internetOptionRefresh         = 37
		internetOptionSettingsChanged = 39
	)
	for _, option := range []uintptr{internetOptionSettingsChanged, internetOptionRefresh} {
		result, _, callErr := internetSetOptionW.Call(0, option, 0, 0)
		if result == 0 {
			return callErr
		}
	}
	return nil
}

func NewPlatformManager(stateRoot string) (*Manager, error) {
	return New(filepath.Join(stateRoot, "private-access", "system-proxy.json"), NewWindowsAdapter(currentUserRegistry{}))
}

func validateProxyJournal(file *os.File, _ os.FileInfo) error {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private proxy journal is a reparse point")
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return errors.New("current Windows token has no valid user")
	}
	// CreateFile uses the token's default owner when the atomic descriptor does
	// not specify O:. Elevated tokens may select Administrators rather than user.
	var length uint32
	err = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &length)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		if err != nil {
			return err
		}
		return errors.New("Windows token owner query returned no buffer")
	}
	if uintptr(length) < unsafe.Sizeof(uintptr(0)) {
		return errors.New("Windows token owner buffer is invalid")
	}
	ownerBuffer := make([]byte, length)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &ownerBuffer[0], length, &length); err != nil {
		return err
	}
	owner := *(**windows.SID)(unsafe.Pointer(&ownerBuffer[0]))
	matches, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, owner)
	runtime.KeepAlive(ownerBuffer)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("private proxy journal has an untrusted owner")
	}
	descriptor := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		descriptor += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	matches, err = windowssecurity.CheckProtectedHandleDACLMatches(handle, descriptor)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("private proxy journal has an untrusted ACL")
	}
	return nil
}

// Journal publication already uses atomicfile MOVEFILE_WRITE_THROUGH. Windows
// directory FlushFileBuffers is unsupported, as in the hoststate store. A
// stale removed journal is safe to reconcile against its exact captured prior.
func syncProxyDirectory(string) error { return nil }

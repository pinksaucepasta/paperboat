//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// Full removal retires diagnostics for the exact account and empty shared
// scaffolding. Reset and nonempty shared directories retain their ownership.
func purgeWindowsSharedResidue(ctx context.Context, shared service.Layout, programData, instance string) (resultErr error) {
	if !validWindowsInstance(instance) {
		return ErrInvalidRequest
	}
	program, err := openWindowsSharedCleanupObject(ctx, shared.InstallRoot, true, true)
	if err != nil {
		return err
	}
	if program != nil {
		defer func() { resultErr = errors.Join(resultErr, program.Close()) }()
	}
	state, err := openWindowsSharedCleanupObject(ctx, programData, true, true)
	if err != nil {
		return err
	}
	if state != nil {
		defer func() { resultErr = errors.Join(resultErr, state.Close()) }()
	}
	// The producers in hostruntimecmd use these exact account-scoped names.
	// No wildcard or neighboring account's diagnostic grants removal authority.
	if state != nil {
		for _, base := range []string{"PaperboatHostd", "PaperboatHostd-worker", "PaperboatUpdated", "PaperboatLocalDaemon", "PaperboatUpdateActivator"} {
			path := filepath.Join(programData, base+"-"+instance+"-startup-error.log")
			resultErr = errors.Join(resultErr, retireWindowsSharedObject(ctx, path, false))
		}
		for _, path := range []string{filepath.Join(programData, "services"), filepath.Join(programData, "users"), filepath.Join(shared.UpdateStateRoot, "users"), shared.UpdateStateRoot} {
			resultErr = errors.Join(resultErr, retireWindowsSharedObject(ctx, path, true))
		}
	}
	if program != nil {
		resultErr = errors.Join(resultErr, retireWindowsSharedObject(ctx, filepath.Join(shared.InstallRoot, "users"), true))
		resultErr = errors.Join(resultErr, markEmptyWindowsSharedDirectory(ctx, program))
	}
	if state != nil {
		resultErr = errors.Join(resultErr, markEmptyWindowsSharedDirectory(ctx, state))
	}
	return resultErr
}

func openWindowsSharedCleanupObject(ctx context.Context, path string, directory, requireProtected bool) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rejectWindowsReparseAncestors(path); err != nil {
		return nil, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE, 0, nil, windows.OPEN_EXISTING, flags, 0)
	if installNotExistOnly(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*os.File, error) { return nil, errors.Join(cause, windows.CloseHandle(handle)) }
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fail(err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || !directory && info.NumberOfLinks != 1 {
		return fail(ErrInvalidRequest)
	}
	final, err := windowsFinalPath(handle)
	if err != nil || !strings.EqualFold(final, windowsExpectedFinalPath(path)) {
		return fail(errors.Join(err, ErrInvalidRequest))
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fail(err)
	}
	if err := checkSharedProgramObjectDescriptor(descriptor, requireProtected); err != nil {
		return fail(err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func retireWindowsSharedObject(ctx context.Context, path string, directory bool) (resultErr error) {
	file, err := openWindowsSharedCleanupObject(ctx, path, directory, false)
	if err != nil || file == nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if directory {
		return markEmptyWindowsSharedDirectory(ctx, file)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	one := byte(1)
	err = windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &one, 1)
	runtime.KeepAlive(file)
	return err
}

func markEmptyWindowsSharedDirectory(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := file.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	one := byte(1)
	err = windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileDispositionInfo, &one, 1)
	runtime.KeepAlive(file)
	// A child created after enumeration remains intact; this is a shared parent.
	if errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) {
		return nil
	}
	return err
}

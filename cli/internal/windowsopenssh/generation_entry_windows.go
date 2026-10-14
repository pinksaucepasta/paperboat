//go:build windows

package windowsopenssh

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// LoadGenerationForInstance resolves the enrolled owner from the protected
// generation declaration. Startup precedes publication of runtime-install.json.
func LoadGenerationForInstance(ctx context.Context, instance, generation string) (result GenerationDescriptor, resultErr error) {
	if ctx == nil || len(instance) != 25 || instance[0] != 'u' || strings.ToLower(instance) != instance || !validSSHGeneration(generation) {
		return result, ErrInvalidConfig
	}
	if _, err := hex.DecodeString(instance[1:]); err != nil {
		return result, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return result, err
	}
	path := filepath.Join(filepath.Dir(layout.UpdateStateRoot), "users", instance, "ssh", "generations", generation, "descriptor.json")
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return result, err
	}
	// Keep the descriptor stable until the existing full reader has validated
	// its parents, exact ACLs, configuration, source bytes and instance binding.
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return result, err
	}
	file := os.NewFile(uintptr(handle), path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return result, err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		return result, ErrServiceOwnership
	}
	if info.FileSizeHigh != 0 || info.FileSizeLow > 16<<10 {
		return result, ErrInvalidConfig
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return result, err
	}
	owned, err := windowssecurity.CheckHandleOwnerMatchesSID(handle, system)
	if err != nil {
		return result, err
	}
	if !owned {
		return result, ErrServiceOwnership
	}
	decoder := json.NewDecoder(io.LimitReader(file, (16<<10)+1))
	decoder.DisallowUnknownFields()
	var declaration GenerationDescriptor
	var extra any
	if decoder.Decode(&declaration) != nil || decoder.Decode(&extra) != io.EOF || declaration.Instance != instance || declaration.Generation != generation || declaration.Validate() != nil {
		return result, ErrInvalidConfig
	}
	matched, err := windowssecurity.CheckProtectedHandleDACLMatches(handle, generationFileDACL(declaration.OwnerSID))
	if err != nil {
		return result, err
	}
	if !matched {
		return result, ErrServiceOwnership
	}
	_, expectedRoot, err := generationPaths(declaration.OwnerSID, generation)
	if err != nil || !sameCleanPath(path, filepath.Join(expectedRoot, "descriptor.json")) {
		return result, ErrServiceOwnership
	}
	verified, err := LoadGeneration(ctx, declaration.OwnerSID, generation)
	if err != nil {
		return result, err
	}
	if verified != declaration {
		return result, ErrServiceOwnership
	}
	return verified, nil
}

package sessionowner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// MaxCatalogEntries is the ceiling of Catalog.Maximum: the configured session
// limit's own ceiling (256) plus one active generation.
const MaxCatalogEntries = 257

// Catalog is the privileged per-account descriptor and native-service owner.
// Maximum reuses the configured session limit plus one active generation.
type Catalog struct {
	Root, ConfigRoot, User, Group, OwnerSID string
	Maximum                                 int
	Runner                                  service.Runner
}

func Find(root, generation string) (string, error) {
	if !absolute(root) || !ValidGeneration(generation) {
		return "", ErrInvalid
	}
	return filepath.Join(root, generation+".json"), nil
}
func (c Catalog) validate() error {
	if !absolute(c.Root) || !absolute(c.ConfigRoot) || c.Maximum < 1 || c.Maximum > MaxCatalogEntries || c.User == "" || c.Group == "" {
		return ErrInvalid
	}
	return nil
}
func (c Catalog) ReadAll(ctx context.Context, instance string) ([]Descriptor, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkCatalogRoot(c.Root, c.OwnerSID); err != nil {
		return nil, err
	}
	directory, err := os.Open(c.Root)
	if err != nil {
		return nil, err
	}
	entries, readErr := directory.ReadDir(c.Maximum + 17)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > c.Maximum+16 {
		return nil, ErrInvalid
	}
	result := make([]Descriptor, 0, c.Maximum)
	staging := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if catalogStagingName(entry.Name()) {
			if entry.IsDir() {
				return nil, ErrInvalid
			}
			staging++
			if staging > 16 {
				return nil, ErrInvalid
			}
			continue
		}
		generation := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Name() != generation+".json" || !ValidGeneration(generation) {
			return nil, ErrInvalid
		}
		d, err := Read(filepath.Join(c.Root, entry.Name()))
		if err != nil {
			return nil, err
		}
		if d.Generation != generation || d.Instance != instance {
			return nil, ErrInvalid
		}
		if len(result) >= c.Maximum {
			return nil, ErrInvalid
		}
		result = append(result, d)
	}
	return result, nil
}
func (c Catalog) Installer(d Descriptor) (*service.Installer, error) { return c.installer(d, false) }
func (c Catalog) installer(d Descriptor, pending bool) (*service.Installer, error) {
	if c.validate() != nil || d.Validate() != nil || d.Source.Platform != runtime.GOOS {
		return nil, ErrInvalid
	}
	controller, err := service.SessionOwnerController(d.Source.Platform, d.Instance, d.Generation, d.UID, c.Runner)
	if err != nil {
		return nil, err
	}
	args := []string{"daemon", "__runtime-session-owner", "--generation", d.Generation}
	if runtime.GOOS == "windows" {
		args = append(args, "--instance", d.Instance)
	}
	config := service.Config{Platform: d.Source.Platform, Kind: service.SessionOwnerKind, Instance: d.Instance, SessionOwnerGeneration: d.Generation, ConfigRoot: c.ConfigRoot, Executable: d.Executable, ExecutableSHA256: d.Source.SHA256, ExecutableLength: d.Source.Length, User: c.User, Group: c.Group, Arguments: args, Environment: map[string]string{"PAPERBOAT_SESSION_OWNER_CATALOG": c.Root, "PAPERBOAT_RUNTIME_STATE_ROOT": d.RuntimeRoot, "PAPERBOAT_WORKSPACE_ROOT": d.Workspace, "PAPERBOAT_SHELL": d.Shell}, Controller: controller}
	if pending {
		return service.NewPending(config)
	}
	return service.New(config)
}
func (c Catalog) Publish(ctx context.Context, d Descriptor) error {
	if c.validate() != nil || d.Validate() != nil {
		return ErrInvalid
	}
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyCatalogExecutable(d); err != nil {
		return err
	}
	if err := prepareCatalogRoot(c.Root, d); err != nil {
		return err
	}
	all, err := c.ReadAll(ctx, d.Instance)
	if err != nil {
		return err
	}
	path, _ := Find(c.Root, d.Generation)
	found := false
	for _, existing := range all {
		if existing.Generation != d.Generation && existing.StoreRoot() == d.StoreRoot() {
			return ErrInvalid
		}
		if existing.Generation == d.Generation {
			if existing.RuntimeRoot != d.RuntimeRoot || existing.InitialStore != d.InitialStore {
				return ErrInvalid
			}
			found = true
			if existing == d {
				return prepareCatalogCapability(d)
			}
			// Never change executable authority while a registered owner may exist.
			controller, e := service.SessionOwnerController(existing.Source.Platform, existing.Instance, existing.Generation, existing.UID, c.Runner)
			if e != nil {
				return e
			}
			installer, e := c.installer(existing, true)
			if e != nil {
				return e
			}
			native, ok := controller.(service.NativeLifecycleController)
			if !ok {
				return ErrInvalid
			}
			status, e := native.Inspect(ctx, installer.DefinitionPath())
			if e != nil {
				return e
			}
			if status.Registered || status.Running {
				return ErrBusy
			}
		}
	}
	if !found && len(all) >= c.Maximum {
		return ErrBusy
	}
	if err := prepareCatalogCapability(d); err != nil {
		return err
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeCatalogDescriptor(path, body, d)
}
func (c Catalog) Install(ctx context.Context, d Descriptor) error {
	if err := c.Publish(ctx, d); err != nil {
		return err
	}
	if err := PrepareNativeEndpoint(d); err != nil {
		return err
	}
	installer, err := c.Installer(d)
	if err != nil {
		return err
	}
	controller, err := service.SessionOwnerController(d.Source.Platform, d.Instance, d.Generation, d.UID, c.Runner)
	if err != nil {
		return err
	}
	native, ok := controller.(service.NativeLifecycleController)
	if !ok {
		return ErrInvalid
	}
	status, err := native.Inspect(ctx, installer.DefinitionPath())
	if err != nil {
		return err
	}
	if status.Running {
		return nil
	}
	if status.Registered {
		if err := installer.PublishDefinition(ctx); err != nil {
			return err
		}
		if err := native.Enable(ctx, installer.DefinitionPath()); err != nil {
			return err
		}
		return native.Start(ctx, installer.DefinitionPath())
	}
	// Preserve published authority on uncertain native failures for recovery.
	return installer.Install(ctx)
}
func (c Catalog) Retire(ctx context.Context, d Descriptor, quiescent func(context.Context) error) error {
	if ctx == nil || quiescent == nil {
		return ErrInvalid
	}
	if err := checkCatalogRoot(c.Root, c.OwnerSID); err != nil {
		return err
	}
	path, err := Find(c.Root, d.Generation)
	if err != nil {
		return err
	}
	published, err := Read(path)
	if err != nil {
		return err
	}
	if published != d {
		return ErrInvalid
	}
	installer, err := c.Installer(d)
	if err != nil {
		return err
	}
	controller, err := service.SessionOwnerController(d.Source.Platform, d.Instance, d.Generation, d.UID, c.Runner)
	if err != nil {
		return err
	}
	native, ok := controller.(service.NativeLifecycleController)
	if !ok {
		return ErrInvalid
	}
	if _, err := verifyRetiredDeclaration(installer, d); err != nil {
		return err
	}
	if err := quiescent(ctx); err != nil {
		return err
	}
	if err := native.Disable(ctx, installer.DefinitionPath()); err != nil {
		return err
	}
	if err := installer.Uninstall(ctx); err != nil {
		return err
	}
	return c.cleanRetiredNative(ctx, d)
}
func newCapability() ([]byte, error) {
	body := make([]byte, 32)
	_, err := rand.Read(body)
	if err != nil {
		return nil, err
	}
	return []byte(base64.RawURLEncoding.EncodeToString(body)), nil
}

// Forget follows authenticated empty-store retirement. It never deletes user
// state and requires native removal before withdrawing the protected descriptor.
func (c Catalog) Forget(ctx context.Context, d Descriptor) error {
	if ctx == nil || c.validate() != nil || d.Validate() != nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkCatalogRoot(c.Root, c.OwnerSID); err != nil {
		return err
	}
	path, err := Find(c.Root, d.Generation)
	if err != nil {
		return err
	}
	published, err := Read(path)
	if err != nil {
		return err
	}
	if published != d {
		return ErrInvalid
	}
	status, err := (Coordinator{Catalog: c, Active: d}).nativeState(ctx, d)
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		return ErrBusy
	}
	return os.Remove(path)
}
func catalogFailure(err error) error { return errors.Join(ErrInvalid, err) }

// InstallerForRemoval validates archive identity while allowing its retired
// executable to be absent; native declaration custody remains authoritative.
func (c Catalog) InstallerForRemoval(d Descriptor) (*service.Installer, error) {
	return c.installer(d, true)
}

func catalogStagingName(name string) bool {
	if strings.HasPrefix(name, ".paperboat-") {
		return ValidGeneration(strings.TrimPrefix(name, ".paperboat-"))
	}
	if len(name) <= 38 || name[0] != '.' || !ValidGeneration(name[1:33]) || name[33:38] != ".json" {
		return false
	}
	for _, c := range name[38:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// HasSource proves a prior generation's presence or its absence under native
// custody. A missing catalogue never bypasses validation of its parent.
func (c Catalog) HasSource(ctx context.Context, instance string, source installsource.Source) (bool, error) {
	if ctx == nil || c.validate() != nil || source.Validate() != nil {
		return false, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := os.Lstat(c.Root); errors.Is(err, os.ErrNotExist) {
		if err := checkCatalogRoot(filepath.Dir(c.Root), c.OwnerSID); err != nil {
			return false, err
		}
		return false, nil
	} else if err != nil {
		return false, err
	}
	all, err := c.ReadAll(ctx, instance)
	if err != nil {
		return false, err
	}
	for _, d := range all {
		if d.Generation == source.SHA256[:32] {
			if d.Source.SHA256 != source.SHA256 || d.Source.Length != source.Length || d.Source.Version != source.Version {
				return false, ErrInvalid
			}
			return true, nil
		}
	}
	return false, nil
}

// cleanRetiredNative is also retried for already absent old owners, so partial
// cleanup never requires restarting a retired generation.
func (c Catalog) cleanRetiredNative(ctx context.Context, d Descriptor) error {
	installer, err := c.installer(d, true)
	if err != nil {
		return err
	}
	// An absent native job may still have a declaration left by a crash
	// between stopping its process and unlinking the exact declaration.
	present, err := verifyRetiredDeclaration(installer, d)
	if err != nil {
		return err
	}
	if present {
		if err := installer.Uninstall(ctx); err != nil {
			return err
		}
	}
	status, err := (Coordinator{Catalog: c, Active: d}).nativeState(ctx, d)
	if err != nil {
		return err
	}
	if status.Registered || status.Running {
		return ErrBusy
	}
	return errors.Join(removeRetiredEndpoint(d), installer.RemoveRetiredSessionOwnerLog())
}

func verifyRetiredDeclaration(installer *service.Installer, d Descriptor) (bool, error) {
	path := installer.DefinitionPath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, ErrInvalid
	}
	if err := validateRetiredDeclaration(path, info, d); err != nil {
		return false, err
	}
	if err := installer.VerifyPublishedDefinition(); err != nil {
		return false, err
	}
	return true, nil
}

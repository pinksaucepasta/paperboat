//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
)

// candidateSessionOwner is the session owner an install attempt starts for its
// source (prepareSessionOwner): its unit, process and catalogue descriptor.
type candidateSessionOwner struct {
	catalog    sessionowner.Catalog
	generation string
	existed    bool // its descriptor was published before this attempt
}

func newCandidateSessionOwner(request Request) (candidateSessionOwner, error) {
	catalog, _, err := sessionOwnerCatalog(request)
	if err != nil {
		return candidateSessionOwner{}, err
	}
	generation, err := sourceGeneration(request.Source)
	if err != nil {
		return candidateSessionOwner{}, err
	}
	path, err := sessionowner.Find(catalog.Root, generation)
	if err != nil {
		return candidateSessionOwner{}, err
	}
	_, err = os.Lstat(path)
	if err != nil && !installNotExistOnly(err) {
		return candidateSessionOwner{}, err
	}
	return candidateSessionOwner{catalog: catalog, generation: generation, existed: err == nil}, nil
}

// withdrawUnless undoes the candidate after an attempt that did not commit: its unit
// is stopped and removed and its descriptor forgotten (pbvm1: every update that
// failed at service apply left one running). A generation that was already
// published, or that the installation record now names (committed), is not this attempt's.
func (o candidateSessionOwner) withdrawUnless(ctx context.Context, committed string) error {
	if o.existed || o.generation == "" || o.generation == committed {
		return nil
	}
	path, err := sessionowner.Find(o.catalog.Root, o.generation)
	if err != nil {
		return err
	}
	// Read wraps a missing descriptor in ErrInvalid, which installNotExistOnly fails closed on: look first. An attempt that
	// died before it published has nothing to withdraw; reporting that as a failure wedged every later install (X lane,
	// Windows: install_recovery kept failing on the never-published candidate).
	if _, err := os.Lstat(path); installNotExistOnly(err) {
		return nil
	}
	d, err := sessionowner.Read(path)
	if err != nil {
		return err
	}
	installer, err := o.catalog.InstallerForRemoval(d)
	if err != nil {
		return err
	}
	if err := installer.Uninstall(ctx); err != nil {
		return err
	}
	return o.catalog.Forget(ctx, d)
}

// replaceCandidate runs one replacement of an installation by a candidate version. The machine guard moves to the
// candidate inside the lifecycle transaction. When the attempt fails, the lifecycle has restored the previous
// version by the time lifecycleReplace returns; then withdraw removes what the attempt published for the
// candidate and the replaced guard is put back, so no component is left on the candidate's version.
func replaceCandidate(ctx context.Context, lifecycleReplace func(context.Context, func(context.Context) error) error, installGuard func(context.Context) (guardUndo, error), replace, withdraw func(context.Context) error) error {
	var guard guardUndo
	err := lifecycleReplace(ctx, func(ctx context.Context) error {
		undo, err := installGuard(ctx)
		if err != nil {
			return err
		}
		guard = undo
		return replace(ctx)
	})
	if err != nil {
		err = errors.Join(err, withdraw(context.WithoutCancel(ctx)))
	}
	return errors.Join(err, finishMachineGuard(ctx, guard, err))
}

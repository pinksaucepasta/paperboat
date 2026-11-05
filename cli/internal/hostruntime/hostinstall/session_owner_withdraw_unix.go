//go:build linux || darwin

package hostinstall

import (
	"context"
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

// withdraw undoes the candidate after an attempt that did not commit: its unit
// is stopped and removed and its descriptor forgotten (pbvm1: every update that
// failed at service apply left one running). A generation that was already
// published, or that the installation record now names, is not this attempt's.
// The inventory that ends the same install removes its endpoint and pin.
func (o candidateSessionOwner) withdraw(ctx context.Context, metadata string, uid int) error {
	if o.existed || o.generation == "" {
		return nil
	}
	installed, err := loadInstallMetadata(metadata, uid)
	if err == nil && installed.SessionOwnerGeneration == o.generation {
		return nil
	}
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	path, err := sessionowner.Find(o.catalog.Root, o.generation)
	if err != nil {
		return err
	}
	d, err := sessionowner.Read(path)
	if installNotExistOnly(err) {
		return nil
	}
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

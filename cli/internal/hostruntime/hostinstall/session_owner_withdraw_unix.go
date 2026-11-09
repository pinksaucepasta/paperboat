//go:build linux || darwin

package hostinstall

import "context"

// withdraw is withdrawUnless against the installation record at metadata.
// The inventory that ends the same install removes the candidate's endpoint and pin.
func (o candidateSessionOwner) withdraw(ctx context.Context, metadata string, uid int) error {
	if o.existed || o.generation == "" {
		return nil
	}
	installed, err := loadInstallMetadata(metadata, uid)
	if err != nil && !installNotExistOnly(err) {
		return err
	}
	return o.withdrawUnless(ctx, installed.SessionOwnerGeneration)
}

//go:build linux || darwin || windows

package hostinstall

import (
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/releaseindex"
)

// InstallationTarget is local update metadata, independent of enrollment.
// Custom installations retain only their inspected source identity.
func InstallationTarget(source installsource.Source) bootstrap.ArtifactTarget {
	if source.Distribution != installsource.Official {
		return bootstrap.ArtifactTarget{}
	}
	repository, _ := buildinfo.ReleaseRepository()
	return bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1,
		Kind: bootstrap.ArtifactKindPB, Version: source.Version, Platform: source.Platform,
		Architecture: source.Architecture, RepositoryURL: repository,
		TargetPath: releaseindex.AssetName(source.Platform, source.Architecture)}
}

package workerupdate

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"

// InstalledBaseline verifies administrator-approved installed bytes without
// claiming they were derived from signed update metadata. Candidate releases
// still come exclusively from TUF and cannot carry LocalSource.
func InstalledBaseline(source installsource.Source, path string) (Release, error) {
	if err := source.Verify(path); err != nil {
		return Release{}, err
	}
	r := Release{LocalSource: &source, Version: source.Version, SHA256: source.SHA256, Length: source.Length, Platform: source.Platform, Architecture: source.Architecture, HostdAPIMin: 1, HostdAPIMax: 1, RuntimeAPIMin: 1, RuntimeAPIMax: 1}
	return completeJournalRelease(r)
}

func validLocalRelease(r Release) bool {
	s := r.LocalSource
	return s != nil && s.Validate() == nil && s.Version == r.Version && s.SHA256 == r.SHA256 && s.Length == r.Length && s.Platform == r.Platform && s.Architecture == r.Architecture && r.ManifestSHA256 == ""
}

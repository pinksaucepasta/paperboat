package updated

import "github.com/pinksaucepasta/paperboat/internal/buildinfo"

// recoveryRepository is the signed repository that vouches for the installed version. An installation that still
// awaits enrollment records none (pairing brings the server's): `pb install` verified it against this build's own
// repository, so that one answers.
func recoveryRepository(recorded string, enrollmentPending bool) (string, error) {
	if recorded == "" && enrollmentPending {
		return buildinfo.ReleaseRepository()
	}
	return recorded, nil
}

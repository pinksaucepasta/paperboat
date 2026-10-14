package hostinstall

import "errors"

// ErrUpdateVerificationFailed is emitted only by pre-install official proof
// validation. It exposes no repository content or underlying filesystem details.
var ErrUpdateVerificationFailed = errors.New("The official update could not be verified; the installed version is unchanged. Download a fresh official update bundle and retry. Force cannot bypass verification.")

// ErrSignedUpdatePending preserves the signed helper's exclusive native recovery
// ownership across local and elevated command boundaries.
var ErrSignedUpdatePending = errors.New("an approved signed update owns native recovery; wait for its update service to finish, then retry")

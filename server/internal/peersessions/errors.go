package peersessions

import "errors"

var (
	ErrInvalid       = errors.New("peer network request is invalid")
	ErrConflict      = errors.New("peer network operation conflicts with its original request")
	ErrUnavailable   = errors.New("peer network authority is unavailable")
	ErrResourceLimit = errors.New("peer network configuration resource limit exceeded")
	ErrDenied        = errors.New("peer network authority denied")
)

type unavailableCause struct{ cause error }

func (e unavailableCause) Error() string        { return ErrUnavailable.Error() }
func (e unavailableCause) Unwrap() error        { return e.cause }
func (e unavailableCause) Is(target error) bool { return target == ErrUnavailable }

func unavailableWithCause(cause error) error {
	if cause == nil {
		return ErrUnavailable
	}
	return unavailableCause{cause: cause}
}

func bounded(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character == 0 || character == '\r' || character == '\n' {
			return false
		}
	}
	return true
}

func equalBytes(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	var different byte
	for index := range left {
		different |= left[index] ^ right[index]
	}
	return different == 0
}

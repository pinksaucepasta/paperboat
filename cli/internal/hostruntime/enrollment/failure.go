package enrollment

import (
	"errors"
	"io"
	"net/http"
)

type enrollmentFailure struct {
	classification error
	cause          error
}

func (failure enrollmentFailure) Error() string        { return failure.classification.Error() }
func (failure enrollmentFailure) Unwrap() error        { return failure.cause }
func (failure enrollmentFailure) Is(target error) bool { return target == failure.classification }

func unavailableEnrollment(cause error) error {
	return enrollmentFailure{classification: ErrUnavailable, cause: cause}
}

var errEnrollmentRedirect = errors.New("runtime enrollment redirect refused")

// A usable credential must not survive a failed response read or cleanup.
func readEnrollmentResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("runtime enrollment response body is missing")
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	closeErr := response.Body.Close()
	if len(raw) > 64<<10 {
		readErr = errors.Join(readErr, ErrInvalid)
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	return raw, nil
}

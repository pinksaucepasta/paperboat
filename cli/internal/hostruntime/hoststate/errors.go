package hoststate

// safeStoreError names the failed step without echoing snapshot content; causes stay reachable
// through errors.Is.
type safeStoreError struct {
	message string
	causes  []error
}

func (err safeStoreError) Error() string { return err.message }

func (err safeStoreError) Unwrap() []error {
	causes := make([]error, len(err.causes))
	copy(causes, err.causes)
	return causes
}

func safeStoreFailure(message string, causes ...error) error {
	retained := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			retained = append(retained, cause)
		}
	}
	return safeStoreError{message: message, causes: retained}
}

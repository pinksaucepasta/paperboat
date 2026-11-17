package safe

import "time"

// NormalizeTime returns value in UTC without a monotonic reading; the zero time stays zero.
func NormalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Round(0)
}

// CloneTime returns an independent copy of value.
func CloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// TimeValue returns *value, or the zero time for nil.
func TimeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

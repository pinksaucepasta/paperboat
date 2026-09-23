package splitdns

import (
	"errors"
	"strings"
)

var reservedSuffixes = map[string]struct{}{
	"alt": {}, "example": {}, "home": {}, "internal": {}, "invalid": {},
	"local": {}, "localhost": {}, "onion": {}, "test": {},
}

var ianaTLDs = func() map[string]struct{} {
	values := strings.Fields(ianaTLDData)
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}()

func ValidateSuffix(suffix string) (string, error) {
	if suffix != strings.TrimSpace(suffix) || len(suffix) < 2 || len(suffix) > 16 {
		return "", errors.New("splitdns suffix must contain 2 to 16 lowercase ASCII letters")
	}
	for _, character := range suffix {
		if character < 'a' || character > 'z' {
			return "", errors.New("splitdns suffix must contain 2 to 16 lowercase ASCII letters")
		}
	}
	if _, reserved := reservedSuffixes[suffix]; reserved {
		return "", errors.New("splitdns suffix is reserved")
	}
	if _, delegated := ianaTLDs[suffix]; delegated {
		return "", errors.New("splitdns suffix conflicts with an IANA root-zone top-level domain")
	}
	return suffix, nil
}

func validateSuffix(suffix string) (string, error) { return ValidateSuffix(suffix) }

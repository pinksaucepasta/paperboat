package buildinfo

import (
	"errors"
	"net/url"
	"strings"
)

// ReleaseRepository comes from the binary's build, never its paired server.
func ReleaseRepository() (string, error) {
	base, err := url.Parse(strings.TrimSpace(DefaultReleaseURL))
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("this build has no valid official release host; install a supplied build with pb update --from")
	}
	return strings.TrimRight(base.String(), "/") + "/tuf", nil
}

package state

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

type issuerError struct{ cause error }

func (e *issuerError) Error() string { return "invalid Paperboat server URL" }
func (e *issuerError) Unwrap() error { return e.cause }
func NormalizeIssuer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", &issuerError{cause: err}
	}
	if u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", errors.New("invalid Paperboat server URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port == "" {
		if strings.Contains(hostname, ":") {
			u.Host = "[" + hostname + "]"
		} else {
			u.Host = hostname
		}
	} else {
		u.Host = net.JoinHostPort(hostname, port)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

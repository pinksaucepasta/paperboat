package connectorprotocol

import (
	"errors"
	"strings"
)

// HTTP01Response is one ACME http-01 key authorization. An edge serves it at
// /.well-known/acme-challenge/<Token> for Hostname only. It is not authority:
// the certificate authority checks the response it fetches.
type HTTP01Response struct {
	Hostname         string `json:"hostname"`
	Token            string `json:"token"`
	KeyAuthorization string `json:"key_authorization"`
}

const (
	MaxHTTP01Responses           = 256
	MaxHTTP01TokenLength         = 256
	MaxHTTP01KeyAuthorizationLen = 512
)

var ErrHTTP01ResponseInvalid = errors.New("invalid HTTP-01 challenge response")

// Validate accepts only an exact lowercase hostname and an ACME token
// (base64url) with its key authorization in the form "<token>.<thumbprint>".
func (r HTTP01Response) Validate() error {
	if r.Hostname == "" || len(r.Hostname) > 253 || r.Hostname != strings.ToLower(r.Hostname) || strings.ContainsAny(r.Hostname, "*/\\:@ \r\n\x00") {
		return ErrHTTP01ResponseInvalid
	}
	if r.Token == "" || len(r.Token) > MaxHTTP01TokenLength || strings.ContainsFunc(r.Token, notBase64URL) {
		return ErrHTTP01ResponseInvalid
	}
	if len(r.KeyAuthorization) > MaxHTTP01KeyAuthorizationLen || !strings.HasPrefix(r.KeyAuthorization, r.Token+".") || len(r.KeyAuthorization) == len(r.Token)+1 {
		return ErrHTTP01ResponseInvalid
	}
	return nil
}

func notBase64URL(c rune) bool {
	return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_')
}

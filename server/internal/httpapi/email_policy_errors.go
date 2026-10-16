package httpapi

import (
	"errors"
	"net/http"

	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

func writeEmailPolicyError(w http.ResponseWriter, r *http.Request, err error) bool {
	status, code := 0, ""
	var policy error
	switch {
	case errors.Is(err, emailpolicy.ErrPlusAddress):
		status, code, policy = 400, "email_alias_not_allowed", emailpolicy.ErrPlusAddress
	case errors.Is(err, emailpolicy.ErrDuplicate):
		status, code, policy = 409, "email_already_registered", emailpolicy.ErrDuplicate
	case errors.Is(err, emailpolicy.ErrInvalid):
		status, code, policy = 400, "invalid_email", emailpolicy.ErrInvalid
	default:
		return false
	}
	logHTTPRejection(r, status, code, policy)
	writeError(w, r, status, code, policy.Error())
	return true
}

package main

// accessTunnelJSONFailure is the typed --json code of a pb access tunnel failure, so scripts branch on the code and
// not on the text. ok is false for every other error.
func accessTunnelJSONFailure(err error) (code, category string, retryable, ok bool) {
	owner := soleCommandOwner(err) // a refusal joined to an independent failure has no sole owner
	if cause, isCause := owner.(accessTunnelCauseError); isCause {
		owner = cause.classification
	}
	switch owner {
	case ErrAccessTunnelAuthentication:
		return "authentication_required", "authentication", false, true
	case ErrAccessTunnelForbidden:
		return "access_forbidden", "authorization_or_entitlement", false, true
	case ErrAccessTunnelRuntimeRPCUnavailable:
		return "access_tunnel_unsupported", "usage", false, true
	case ErrAccessTunnelUnavailable:
		return "access_tunnel_unavailable", "unavailable_retryable", true, true
	case ErrAccessTunnelGone:
		return "access_tunnel_session_gone", "unavailable_retryable", true, true
	case ErrAccessTunnelInvalid:
		return "access_tunnel_unexpected_response", "local_io", false, true
	}
	return "", "", false, false
}

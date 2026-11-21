package breakglass

// sealedTargets are every column holding a ciphertext of the encryption key ring (the tables
// server/internal/secrets callers write). Re-sealing needs only the table, a ctid and the column: none of them has
// associated data or a keyed companion column. Two ciphertext holders are left alone on purpose: the receipt of an
// operator infrastructure enrollment embeds its credential ciphertext in JSON (an idempotency replay window, it
// fails closed once the old key is gone), and GitHub repository cursors are handed to clients and expire with them.
var sealedTargets = []struct{ table, column string }{
	{"sessions", "workos_refresh_token_ciphertext"},
	{"device_token_redemptions", "response_ciphertext"},
	{"client_refresh_redemptions", "response_ciphertext"},
	{"user_machine_pairings", "bootstrap_token_ciphertext"},
	{"user_machine_pairings", "installation_config_ciphertext"},
	{"control_helper_enrollments", "grant_ciphertext"},
	{"helper_identity_renewals", "identity_ciphertext"},
	{"control_config_credentials", "credential_ciphertext"},
	{"control_config_repository_access_operations", "access_ciphertext"},
	{"api_token_create_requests", "secret_ciphertext"},
	{"billing_workspace_operations", "request_ciphertext"},
	{"billing_workspace_operations", "result_ciphertext"},
	{"team_buckets", "secret"},
	{"control_server_node_acme_cache", "sealed"},
	{"managed_ssh_user_cas", "private_key_ciphertext"},
	{"selfhost_installations", "runtime_ciphertext"},
	{"github_oauth_tokens", "token_ciphertext"},
	{"github_oauth_tokens", "refresh_token_ciphertext"},
	{"team_receipt_smtp", "password_ciphertext"},
	{"operator_totp", "secret_cipher"},
	{"audit_stream_destinations", "secrets"},
}

// revocations end every credential an account holds (the statements of operatoractions revokeUserAuthority, over
// all accounts instead of one). Each only touches rows created before the stage began, so a person who signs in
// again during the run keeps the new session.
var revocations = []struct{ name, table, live, created, set string }{
	{"browser_sessions", "sessions", `revoked_at IS NULL`, "created_at", `revoked_at = now(), updated_at = now(), version = version + 1`},
	{"browser_access_sessions", "browser_access_sessions", `revoked_at IS NULL`, "issued_at", `revoked_at = now()`},
	{"cli_sessions", "cli_client_sessions", `state = 'active'`, "created_at", `state = 'revoked', revoked_at = now(), revocation_reason = 'break_glass', version = version + 1`},
	{"cli_access_tokens", "cli_access_tokens", `revoked_at IS NULL`, "created_at", `revoked_at = now()`},
	{"cli_refresh_tokens", "cli_refresh_tokens", `state <> 'revoked'`, "created_at", `state = 'revoked', revoked_at = now()`},
	{"api_tokens", "api_tokens", `revoked_at IS NULL`, "created_at", `revoked_at = now(), revoked_by = 'break_glass'`},
	{"recipes", "recipes", `revoked_at IS NULL`, "created_at", `revoked_at = now(), revoked_by = 'break_glass'`},
	{"device_grants", "device_grants", `state IN ('pending', 'approved')`, "issued_at", `state = 'denied', denied_at = now(), version = version + 1`},
	// A breach exposes unused enrollment tokens (`pb machine add`) and unredeemed install material; owners issue new ones.
	// The same end states the product gives a pairing when its machine is removed (machine_cleanup.sql).
	{"enrollment_tokens", "user_machine_pairings", `installation_state = 'awaiting_bootstrap' AND bootstrap_token_hash IS NOT NULL`, "created_at", `installation_state = 'deleted', bootstrap_token_hash = NULL, bootstrap_token_ciphertext = NULL, expires_at = least(expires_at, now()), updated_at = now()`},
	{"pairing_install_material", "user_machine_pairings", `state IN ('pending', 'approved') AND installation_config_ciphertext IS NOT NULL`, "created_at", `state = 'denied', denied_at = now(), installation_state = 'cancelled', installation_config_ciphertext = NULL, installation_config_nonce = NULL, installation_config_consumed_at = NULL, installation_recovery_operation_key = NULL, expires_at = least(expires_at, now()), updated_at = now()`},
	{"machine_access_sessions", "user_machine_access_sessions", `state = 'active'`, "created_at", `state = 'revoked', revoked_at = now(), revocation_reason = 'break_glass', updated_at = now()`},
}

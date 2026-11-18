package breakglass

// Provider is a third-party credential the server holds. Paperboat cannot rotate these: the operator does, at the
// provider, then deploys the new value. The list is the runbook's third-party section (~/paperboat-private).
type Provider struct {
	Name  string `json:"name"`
	What  string `json:"what"`
	Where string `json:"where"`
	Link  string `json:"link"`
}

// ThirdParty is the manual checklist shown with the preview and after the run.
func ThirdParty() []Provider {
	return []Provider{
		{"WorkOS", "API key, client id and actions secret", "WorkOS dashboard, API keys and webhooks", "https://dashboard.workos.com"},
		{"GitHub App and OAuth", "App private key, OAuth client secret", "GitHub, Developer settings", "https://github.com/settings/apps"},
		{"Stripe", "API key and webhook signing secret", "Stripe dashboard, Developers, API keys and Webhooks", "https://dashboard.stripe.com/apikeys"},
		{"Resend", "API key", "Resend, API keys", "https://resend.com/api-keys"},
		{"Cloudflare and ACME", "DNS API token, ACME account key", "Cloudflare, My Profile, API tokens", "https://dash.cloudflare.com/profile/api-tokens"},
		{"Diagnostics S3", "access key pair of the diagnostics bucket", "the object storage provider's access keys", ""},
		{"Sentry and GlitchTip", "auth token, DSNs", "Sentry, Settings, Auth tokens", "https://sentry.io/settings/auth-tokens/"},
		{"PostHog", "personal API key, project key", "PostHog, Personal API keys", "https://app.posthog.com/settings/user-api-keys"},
		{"Database", "role password and DSN", "the database provider, then PAPERBOAT_DATABASE_DSN", ""},
		{"Release SSH key", "write access to the release host", "GitHub environment paperboat-tuf-published secrets and the release host's authorized_keys", ""},
	}
}

/**
 * Optional integrations: each answers "not configured" (nothing to fix, say so in one line) and "down" (a short error
 * with the support reference) with different codes. The server's codes, by provider.
 */
const down: Record<string, string> = {
  analytics_unavailable: "PostHog",
  money_unavailable: "Stripe",
  operator_evidence_unavailable: "Sentry",
  sentry_rate_limited: "Sentry",
}

export type IntegrationState = { kind: "not_configured" } | { kind: "down"; provider: string } | { kind: "other" }

export function integrationState(code: string): IntegrationState {
  if (code.endsWith("_not_configured")) return { kind: "not_configured" }
  const provider = down[code]
  return provider ? { kind: "down", provider } : { kind: "other" }
}

/** One line for a section that has nothing to show because its integration is not set up. */
export const notConfiguredLine: Record<string, string> = {
  money_not_configured: "Stripe isn’t set up on this server (PAPERBOAT_STRIPE_API_KEY).",
  analytics_not_configured: "PostHog isn’t set up on this server (PAPERBOAT_POSTHOG_*).",
  sentry_not_configured: "Sentry isn’t set up on this server (PAPERBOAT_SENTRY_*).",
  release_authority_not_configured: "Release authority isn’t set up on this server.",
}

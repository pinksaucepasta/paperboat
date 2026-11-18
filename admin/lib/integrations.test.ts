import { expect, test } from "bun:test"
import { integrationState, notConfiguredLine } from "@/lib/integrations"

// Both states of every optional integration the panel reads: unset is not an error, down is a different one.
const integrations = [
  { name: "Stripe", unset: "money_not_configured", down: "money_unavailable" },
  { name: "PostHog", unset: "analytics_not_configured", down: "analytics_unavailable" },
  { name: "Sentry", unset: "sentry_not_configured", down: "operator_evidence_unavailable" },
]

for (const i of integrations) {
  test(`${i.name}: unset and down are told apart`, () => {
    expect(integrationState(i.unset)).toEqual({ kind: "not_configured" })
    expect(integrationState(i.down)).toEqual({ kind: "down", provider: i.name })
    expect(notConfiguredLine[i.unset]).toContain(i.name)
  })
}

test("release authority is only ever set or unset", () => {
  expect(integrationState("release_authority_not_configured").kind).toBe("not_configured")
})

test("a rate limit is told apart from an outage", () => {
  expect(integrationState("sentry_rate_limited")).toEqual({ kind: "rate_limited", provider: "Sentry" })
})

test("an ordinary failure is neither", () => {
  expect(integrationState("forbidden")).toEqual({ kind: "other" })
  expect(integrationState("timeout")).toEqual({ kind: "other" })
})

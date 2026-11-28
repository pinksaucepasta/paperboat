import { describe, expect, test } from "bun:test"
import {
  isLimitedPublicRoute,
  isViewerEntry,
  savedViewers,
  viewerListProblem,
  viewerSaveMessage,
  viewerMutation,
  type ViewerPolicyView,
} from "./viewer-access"

const policy = (over: Partial<ViewerPolicyView>): ViewerPolicyView => ({
  configured: true,
  native_access_mode: "private",
  ...over,
})

describe("viewer entries", () => {
  test("takes addresses and *@domain", () => {
    for (const entry of ["user@x.com", "a+tag@sub.company.com", "*@company.com", "*@bücher.example"])
      expect(isViewerEntry(entry)).toBe(true)
  })
  test("refuses a bare or partial wildcard and anything that is not one address", () => {
    for (const entry of [
      "",
      "*",
      "*@*",
      "*@com",
      "*@company.",
      "*@.company.com",
      "*@*.company.com",
      "a*@company.com",
      "*company.com",
      "company.com",
      "@company.com",
      "a@b@company.com",
      "a b@company.com",
      "a@company",
      "a@company..com",
      "Name <a@company.com>",
      "*@company.com/path",
      `${"a".repeat(250)}@x.com`,
    ])
      expect(isViewerEntry(entry)).toBe(false)
  })
  test("names what cannot be saved", () => {
    expect(viewerListProblem(["a@x.com", "*"])).toContain("*")
    expect(viewerListProblem(Array.from({ length: 101 }, (_, i) => `u${i}@x.com`))).toContain("100")
    expect(viewerListProblem(["a@x.com", "*@x.com"])).toBeNull()
  })
})

describe("viewer policy", () => {
  test("reads the saved list of each audience", () => {
    expect(savedViewers(policy({ audience: "specific_emails", allowed_emails: ["a@x.com"] }))).toEqual(["a@x.com"])
    expect(savedViewers(policy({ audience: "email_domain", email_domain: "x.com" }))).toEqual(["*@x.com"])
    expect(savedViewers(policy({ audience: "owner" }))).toEqual([])
    expect(savedViewers(policy({ configured: false }))).toEqual([])
  })
  test("never saves a public audience from a list", () => {
    expect(viewerMutation([])).toEqual({ audience: "owner" })
    expect(viewerMutation(["*@x.com"])).toEqual({ audience: "specific_emails", allowed_emails: ["*@x.com"] })
  })
  test("shows a public route only while a policy limits it", () => {
    const open = { native_access_mode: "public" as const }
    expect(isLimitedPublicRoute(policy({ ...open, audience: "specific_emails" }))).toBe(true)
    expect(isLimitedPublicRoute(policy({ ...open, audience: "public" }))).toBe(false)
    expect(isLimitedPublicRoute(policy({ ...open, configured: false }))).toBe(false)
    expect(isLimitedPublicRoute(policy({ audience: "owner" }))).toBe(false)
  })
})

describe("viewer save failures", () => {
  test("says how to limit a live public preview instead of the server sentence", () => {
    expect(viewerSaveMessage({ code: "edge_policy_transition_required" })).toBe(
      "To limit who can open this preview, publish it again with --allow <emails/domains>.",
    )
    expect(viewerSaveMessage({ code: "viewer_policy_conflict" })).toBeNull()
    expect(viewerSaveMessage(new Error("boom"))).toBeNull()
  })
})

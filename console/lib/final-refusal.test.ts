import { expect, test } from "bun:test"
import { ApiError, isFinalRefusal } from "@/lib/api"

test("only a 403 or the quota 409 is a refusal the page words itself; the quota code on another status still retries", () => {
  expect(isFinalRefusal(new ApiError(403, "sso_required", "x"))).toBe(true)
  expect(isFinalRefusal(new ApiError(409, "bandwidth_quota_exhausted", "x"))).toBe(true)
  expect(isFinalRefusal(new ApiError(429, "bandwidth_quota_exhausted", "x"))).toBe(false)
  expect(isFinalRefusal(new ApiError(400, "bandwidth_quota_exhausted", "x"))).toBe(false)
  expect(isFinalRefusal(new ApiError(409, "state_changed", "x"))).toBe(false)
  expect(isFinalRefusal(new Error("network"))).toBe(false)
})

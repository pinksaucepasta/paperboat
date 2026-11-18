import { expect, test } from "bun:test"
import { ApiError, isFinalRefusal } from "@/lib/api"

test("only a 403 is a refusal the page words itself; other statuses retry", () => {
  expect(isFinalRefusal(new ApiError(403, "sso_required", "x"))).toBe(true)
  expect(isFinalRefusal(new ApiError(409, "state_changed", "x"))).toBe(false)
  expect(isFinalRefusal(new ApiError(429, "rate_limited", "x"))).toBe(false)
  expect(isFinalRefusal(new Error("network"))).toBe(false)
})

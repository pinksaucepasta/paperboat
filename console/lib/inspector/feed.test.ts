import { describe, expect, test } from "bun:test"

import { afterPolicyLost } from "@/lib/inspector/feed"

// A runtime restart drops the daemon's in-memory capture policy; capture the user had on must come back by itself.
describe("afterPolicyLost", () => {
  test("capture that was on is switched on again", () => expect(afterPolicyLost("on")).toBe("reenable"))
  test("capture that was off or never read stays off", () => {
    expect(afterPolicyLost("off")).toBe("off")
    expect(afterPolicyLost("unknown")).toBe("off")
  })
})

import { expect, test } from "bun:test"
import { sessionVerdict } from "@/lib/session-verdict"

test("only a successful /me proves the session", () => {
  expect(sessionVerdict(200)).toBe("valid")
  expect(sessionVerdict(204)).toBe("valid")
})

test("a 401 from /me ends the session", () => {
  expect(sessionVerdict(401)).toBe("ended")
})

test("an outage is neither a valid session nor a sign-out", () => {
  for (const status of [0, 403, 404, 429, 500, 502, 503, 504]) expect(sessionVerdict(status)).toBe("unknown")
})

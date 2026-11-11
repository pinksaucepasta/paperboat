import { expect, test } from "bun:test"
import { participantsPollMs, terminalQuery } from "@/lib/terminal/queries"

// F21-169: a viewer polls a terminal's details every 4 s until the host pushes who is attached, then at most once a
// minute. 50 viewers at 4 s were 12.5 requests a second to the server.
test("a terminal's details are polled every 4 s until the host pushes participants, then every 60 s", () => {
  expect(participantsPollMs(false)).toBe(4_000)
  expect(participantsPollMs(true)).toBe(60_000)
  expect(terminalQuery("t1").refetchInterval).toBe(4_000)
  expect(terminalQuery("t1", true).refetchInterval).toBe(60_000)
})

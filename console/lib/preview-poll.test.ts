import { afterEach, beforeEach, describe, expect, jest, test } from "bun:test"

import { QueryClient, QueryObserver } from "@tanstack/react-query"
import { previewPoll, previewQuery } from "@/lib/tunnels"

type Body = { state: string; allocation_state: string }
const realFetch = globalThis.fetch
let body: Body
let polls = 0

beforeEach(() => {
  polls = 0
  jest.useFakeTimers()
  globalThis.fetch = (async () => {
    polls++
    return Response.json({ data: body })
  }) as unknown as typeof fetch
})
afterEach(() => {
  jest.useRealTimers()
  globalThis.fetch = realFetch
})

async function advance(ms: number) {
  for (let t = 0; t < ms; t += 100) {
    jest.advanceTimersByTime(100)
    for (let i = 0; i < 20; i++) await Promise.resolve()
  }
}

/** Polls made in the 10 s after the first answer, for a preview that answers `answer` every time. */
async function pollsAfterFirstAnswer(answer: Body, untilReady: boolean) {
  body = answer
  const observer = new QueryObserver(new QueryClient(), {
    ...previewQuery("p1"),
    refetchInterval: previewPoll(1_000, untilReady),
  })
  const stop = observer.subscribe(() => {})
  await advance(100)
  const first = polls
  await advance(10_000)
  stop()
  return { first, after: polls - first }
}

describe("previewPoll", () => {
  test("a pending preview keeps polling", async () => {
    const { first, after } = await pollsAfterFirstAnswer({ state: "pending", allocation_state: "pending" }, true)
    expect(first).toBe(1)
    expect(after).toBeGreaterThanOrEqual(9)
  })

  for (const [name, answer] of [
    ["expired", { state: "expired", allocation_state: "released" }],
    ["stopped", { state: "stopped", allocation_state: "released" }],
    ["failed", { state: "pending", allocation_state: "failed" }],
  ] as const) {
    test(`no poll after ${name}`, async () => {
      expect((await pollsAfterFirstAnswer(answer, false)).after).toBe(0)
    })
  }

  test("no poll after ready when waiting for ready", async () => {
    expect((await pollsAfterFirstAnswer({ state: "ready", allocation_state: "ready" }, true)).after).toBe(0)
  })

  test("a ready preview keeps polling for the inspector", async () => {
    expect((await pollsAfterFirstAnswer({ state: "ready", allocation_state: "ready" }, false)).after).toBeGreaterThan(0)
  })
})

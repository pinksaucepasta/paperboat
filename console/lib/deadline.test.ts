import { describe, expect, test } from "bun:test"
import { withDeadline } from "@/lib/deadline"

describe("withDeadline", () => {
  test("the deadline applies without a caller signal", async () => {
    const signal = withDeadline(undefined, 20)
    await new Promise((resolve) => setTimeout(resolve, 60))
    expect(signal.aborted).toBe(true)
    expect((signal.reason as DOMException).name).toBe("TimeoutError")
  })

  test("a caller's signal keeps the deadline", async () => {
    const caller = new AbortController()
    const signal = withDeadline(caller.signal, 20)
    await new Promise((resolve) => setTimeout(resolve, 60))
    expect(caller.signal.aborted).toBe(false)
    expect(signal.aborted).toBe(true)
    expect((signal.reason as DOMException).name).toBe("TimeoutError")
  })

  test("the caller can still cancel before the deadline", () => {
    const caller = new AbortController()
    const signal = withDeadline(caller.signal, 60_000)
    caller.abort()
    expect(signal.aborted).toBe(true)
    expect((signal.reason as DOMException).name).toBe("AbortError")
  })
})

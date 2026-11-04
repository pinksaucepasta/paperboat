import { afterEach, beforeEach, describe, expect, test } from "bun:test"
import { installationsQuery, poolServersQuery, scopeOf } from "@/lib/network"

const realFetch = globalThis.fetch
const count = 450
const asked: URL[] = []

/** A server holding `count` installations, answering the keyset-paged list as the control plane does. */
beforeEach(() => {
  asked.length = 0
  globalThis.fetch = (async (input: string) => {
    const url = new URL(input, "http://console.test")
    asked.push(url)
    const limit = Math.min(Number(url.searchParams.get("limit")), 200)
    const start = Number(url.searchParams.get("cursor") ?? "0")
    const ids = Array.from({ length: Math.max(0, Math.min(limit, count - start)) }, (_, i) => start + i + 1)
    const next = start + limit < count ? String(start + limit) : undefined
    const data = {
      pagination: { limit, offset: 0, total: count, ...(next && { next_cursor: next }) },
      installations: ids.map((n) => ({ installation_id: `i${n}`, node_id: `n${n}`, capability: "relay" })),
    }
    return Response.json({ data })
  }) as unknown as typeof fetch
})
afterEach(() => {
  globalThis.fetch = realFetch
})

// PQ-04: the Network page shows one server page at a time with the server's total; it never walks the whole list.
describe("installations page on the server", () => {
  test("page 1 is one request; page 2 follows the server's cursor; the total is the server's", async () => {
    const run = (page: number) =>
      installationsQuery(scopeOf(undefined), "", page).queryFn!({ signal: new AbortController().signal } as never)
    const first = await run(1)
    expect(asked).toHaveLength(1)
    expect(asked[0].searchParams.get("cursor")).toBeNull()
    expect(first.items).toHaveLength(50)
    expect(first.total).toBe(count)
    const second = await run(2)
    expect(asked).toHaveLength(2)
    expect(asked[1].searchParams.get("cursor")).toBe("50")
    expect(second.items[0].installation_id).toBe("i51")
  })
})

// The pool picker never stops at its first page: load more follows the server's cursor to the last server.
describe("pool picker reaches every server", () => {
  test("load more walks the cursor past the first page and searches on the server", async () => {
    const options = poolServersQuery("relay", scopeOf(undefined), "edge")
    const fetchPage = (pageParam: string) =>
      options.queryFn!({ pageParam, signal: new AbortController().signal } as never)
    let page = await fetchPage(options.initialPageParam as string)
    let seen = page.items.length
    while (options.getNextPageParam!(page as never, [] as never, "" as never, [] as never)) {
      page = await fetchPage(options.getNextPageParam!(page as never, [] as never, "" as never, [] as never) as string)
      seen += page.items.length
    }
    expect(seen).toBe(count)
    expect(
      asked.every(
        (url) => url.searchParams.get("q") === "edge" && url.searchParams.get("enrollment_state") === "enrolled",
      ),
    ).toBe(true)
  })
})

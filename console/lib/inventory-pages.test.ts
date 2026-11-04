import { afterEach, beforeEach, describe, expect, test } from "bun:test"
import { fetchInstallations, scopeOf } from "@/lib/network"

const realFetch = globalThis.fetch
const count = 450
const asked: string[] = []

/** A server holding `count` items, answering the offset-paged installations list as the control plane does (200 max). */
beforeEach(() => {
  asked.length = 0
  globalThis.fetch = (async (input: string) => {
    asked.push(input)
    const url = new URL(input, "http://console.test")
    const limit = Math.min(Number(url.searchParams.get("limit")), 200)
    const offset = Number(url.searchParams.get("offset"))
    const ids = Array.from({ length: Math.max(0, Math.min(limit, count - offset)) }, (_, i) => offset + i + 1)
    const pagination = { limit, offset, total: count, next_offset: offset + limit < count ? offset + limit : null }
    const data = {
      pagination,
      installations: ids.map((n) => ({ installation_id: `i${n}`, node_id: `n${n}`, capability: "relay" })),
    }
    return Response.json({ data })
  }) as unknown as typeof fetch
})
afterEach(() => {
  globalThis.fetch = realFetch
})

describe("inventories past the first 200", () => {
  test("self-host installations reach installation 450", async () => {
    const installations = await fetchInstallations(scopeOf(undefined))
    expect(installations).toHaveLength(count)
    expect(installations.at(-1)?.installation_id).toBe("i450")
    expect(asked.every((url) => url.includes("limit=200"))).toBe(true)
  })
})

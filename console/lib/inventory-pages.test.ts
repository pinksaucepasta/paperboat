import { afterEach, beforeEach, describe, expect, test } from "bun:test"
import { fetchConfigStatus } from "@/lib/config-sync"
import { fetchInstallations, scopeOf } from "@/lib/network"

const realFetch = globalThis.fetch
const count = 450
const asked: string[] = []

/** A server holding `count` items, answering any offset-paged list as the control plane does (200 max). */
beforeEach(() => {
  asked.length = 0
  globalThis.fetch = (async (input: string) => {
    asked.push(input)
    const url = new URL(input, "http://console.test")
    const limit = Math.min(Number(url.searchParams.get("limit")), 200)
    const offset = Number(url.searchParams.get("offset"))
    const ids = Array.from({ length: Math.max(0, Math.min(limit, count - offset)) }, (_, i) => offset + i + 1)
    const pagination = { limit, offset, total: count, next_offset: offset + limit < count ? offset + limit : null }
    const data = url.pathname.endsWith("config-sync/status")
      ? {
          state: "healthy",
          pagination,
          environments: ids.map((n) => ({
            machine_id: `m${n}`,
            environment_id: `e${n}`,
            alias: `box-${n}`,
            state: n === count ? "conflict" : "healthy",
          })),
        }
      : {
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
  test("config sync status reaches machine 450 and its state", async () => {
    const status = await fetchConfigStatus()
    expect(status.environments).toHaveLength(count)
    expect(status.environments.at(-1)).toMatchObject({ machine_id: "m450", state: "conflict" })
    expect(status.state).toBe("healthy")
  })

  test("self-host installations reach installation 450", async () => {
    const installations = await fetchInstallations(scopeOf(undefined))
    expect(installations).toHaveLength(count)
    expect(installations.at(-1)?.installation_id).toBe("i450")
    expect(asked.every((url) => url.includes("limit=200"))).toBe(true)
  })
})

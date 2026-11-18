import { afterAll, beforeAll, expect, test } from "bun:test"
import { rotateRecordingKey } from "@/lib/recordings"

// The vault worker is replaced by a stub that records what the page asks of it: this test is about what the page
// sends to the worker and to the server, not the crypto (cli/cmd/environmentvault-wasm and cli/internal/recording
// test that).
const asked: Record<string, unknown>[] = []
const vault = (async (request: Record<string, unknown>) => {
  asked.push(request)
  return {
    key: { generation: 2, public_key: "cHVi+/==", signing_public_key: "c2ln+/==", rotation_signature: "c2ln+/==" },
    sealed_previous: "cHJl+/==",
    grants: [{ account_id: "acct_a", sharing_public_key: "c2hh+/==", sealed: "c2Vh+/==" }],
  }
}) as never
const sent: { url: string; body: string }[] = []
const realFetch = globalThis.fetch
beforeAll(() => {
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    sent.push({ url: String(url), body: String(init.body) })
    return new Response(JSON.stringify({ data: { generation: 2, public_key: "cHVi+/==", signing_public_key: "c2ln+/==", rotation_signature: "c2ln+/==" } }), { status: 200, headers: { "content-type": "application/json" } })
  }) as unknown as typeof fetch
})
afterAll(() => {
  globalThis.fetch = realFetch
})

const state = (overrides: Record<string, unknown> = {}) =>
  ({
    key: { generation: 1, public_key: "AAAA", signing_public_key: "BBBB" },
    my_key: { generation: 1, sealed: "c2Vh+/==" },
    viewers: [
      { account_id: "acct_a", sharing_public_key: "c2hh+/==", has_grant: true },
      { account_id: "acct_b", has_grant: false },
    ],
    manual_start: "anyone",
    ...overrides,
  }) as never

test("replacing the key asks the vault worker for generation N+1 from this member's own copy and posts only public parts and sealed copies", async () => {
  asked.length = 0
  sent.length = 0
  await rotateRecordingKey("team_1", state(), vault)
  expect(asked).toHaveLength(1)
  expect(asked[0]).toMatchObject({ action: "recording-key-rotate", team_id: "team_1", generation: 1, sealed: "c2Vh+/==" })
  // Only the viewers with a vault get a copy (the base64url conversion is the worker wrapper's, tested in wasm-boundary.test.ts).
  expect(asked[0].viewers).toEqual([{ account_id: "acct_a", sharing_public_key: "c2hh+/==" }])
  expect(sent).toHaveLength(1)
  expect(sent[0].url).toContain("teams/team_1/recording-key")
  const body = JSON.parse(sent[0].body)
  expect(Object.keys(body).sort()).toEqual(["grants", "key", "sealed_previous"])
  expect(body.key.rotation_signature).toBe("c2ln+/==")
  expect(body.sealed_previous).toBe("cHJl+/==")
  expect(JSON.stringify(body)).not.toMatch(/private|bundle|password/i)
})

test("a member without a copy of the current key, or with an older one, cannot replace it", async () => {
  asked.length = 0
  await expect(rotateRecordingKey("team_1", state({ my_key: undefined }), vault)).rejects.toThrow()
  await expect(rotateRecordingKey("team_1", state({ my_key: { generation: 1, sealed: "x" }, key: { generation: 2, public_key: "A", signing_public_key: "B" } }), vault)).rejects.toThrow()
  await expect(rotateRecordingKey("team_1", state({ viewers: [{ account_id: "acct_b", has_grant: false }] }), vault)).rejects.toThrow()
  expect(asked).toHaveLength(0)
})

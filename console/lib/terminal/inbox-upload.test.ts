import { expect, test } from "bun:test"
import type { BrowserTerminalAPI, BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"
import { describeUploadFailure, openBrowserConnection, type Ticket } from "@/lib/terminal/inbox-upload"
import { ApiError } from "@/lib/api"
import { FilePasteError } from "@/lib/terminal/file-paste"

const fingerprint = "a".repeat(64)
const session = { id: "term_1", ownerAccount: "acct_1", machineId: "machine_1" }
const ticket = (change: Partial<Ticket> = {}): Ticket => ({
  stream_url: "wss://edge.example.test/stream",
  edge_node_id: "edge-1",
  subprotocols: ["paperboat.browser-terminal.e2ee.v1", "pb-ticket.abc"],
  expires_at: new Date(Date.now() + 60_000).toISOString(),
  role: "owner",
  machine_id: "machine_1",
  owner_account_id: "acct_1",
  root_key_id: "root_1",
  root_public_key: "key",
  root_fingerprint: fingerprint,
  ...change,
})

/** The worker's API with only what a connection touches, recording how it was used. */
function fakeBrowser() {
  const calls = { discarded: [] as string[], left: [] as string[], config: undefined as Record<string, unknown> | undefined }
  let callback: ((event: BrowserTerminalEvent) => void) | undefined
  const browser = {
    createIdentity: async () => ({ id: "identity_1", public_key_sha256: "sha" }),
    inspectRoot: async () => ({ root_key_id: "root_1", fingerprint }),
    discardIdentity: (id: string) => calls.discarded.push(id),
    leave: (id: string) => calls.left.push(id),
    connect: async (_: string, config: Record<string, unknown>, cb: (event: BrowserTerminalEvent) => void) => {
      calls.config = config
      callback = cb
      return { id: "connection_1" }
    },
  } as unknown as BrowserTerminalAPI
  return { browser, calls, emit: (event: BrowserTerminalEvent) => callback?.(event) }
}

test("a ticket for this terminal opens the connection with the pinned key, and events carry the connection id", async () => {
  const { browser, calls, emit } = fakeBrowser()
  const seen: string[] = []
  const opened = await openBrowserConnection(browser, {
    session,
    fromSequence: "7",
    fetchTicket: async (key) => (expect(key).toBe("sha"), ticket()),
    onEvent: (id, pin) => seen.push(`${id}:${pin.rootKeyID}`),
  })
  expect(opened?.id).toBe("connection_1")
  expect(calls.config).toMatchObject({ terminal_session_id: "term_1", from_sequence: "7", root_key_id: "root_1", role: "owner" })
  expect(calls.discarded).toEqual([]) // the connect took the key
  emit({ type: "attached" })
  expect(seen).toEqual(["connection_1:root_1"])
})

test("a ticket for another owner, machine or an insecure address is refused and the key is discarded", async () => {
  for (const change of [{ owner_account_id: "acct_2" }, { machine_id: "machine_2" }, { stream_url: "ws://edge.example.test/s" }, { role: "admin" as never }]) {
    const { browser, calls } = fakeBrowser()
    await expect(openBrowserConnection(browser, { session, fetchTicket: async () => ticket(change), onEvent: () => undefined })).rejects.toThrow()
    expect(calls.discarded).toEqual(["identity_1"])
    expect(calls.config).toBeUndefined()
  }
})

test("a machine key that does not match the ticket's fingerprint is refused", async () => {
  const { browser, calls } = fakeBrowser()
  await expect(
    openBrowserConnection(browser, { session, fetchTicket: async () => ticket({ root_fingerprint: "b".repeat(64) }), onEvent: () => undefined }),
  ).rejects.toThrow()
  expect(calls.discarded).toEqual(["identity_1"])
})

test("canceling before or after the connect leaves nothing open", async () => {
  const before = fakeBrowser()
  expect(await openBrowserConnection(before.browser, { session, canceled: () => true, fetchTicket: async () => ticket(), onEvent: () => undefined })).toBeNull()
  expect(before.calls.discarded).toEqual(["identity_1"])

  const after = fakeBrowser()
  let asked = 0
  const result = await openBrowserConnection(after.browser, {
    session,
    canceled: () => ++asked > 2, // true only once the connect has finished
    fetchTicket: async () => ticket(),
    onEvent: () => undefined,
  })
  expect(result).toBeNull()
  expect(after.calls.left).toEqual(["connection_1"])
})

test("upload failures are put in words: file errors, plan limit, anything else", () => {
  expect(describeUploadFailure(new FilePasteError("batch_limit"))).toMatch(/limits/)
  expect(describeUploadFailure(new Error("The machine did not answer in time."))).toBe("The machine did not answer in time.")
  expect(describeUploadFailure(new ApiError(403, "browser_terminal_limit_reached", "x"))).toMatch(/browser terminal limit/)
  expect(describeUploadFailure("odd")).toBe("The machine could not be reached.")
})

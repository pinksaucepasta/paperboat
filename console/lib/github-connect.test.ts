import { afterEach, describe, expect, mock, spyOn, test } from "bun:test"
import { focusManager, QueryClient, QueryObserver } from "@tanstack/react-query"

mock.module("server-only", () => ({}))
process.env.PAPERBOAT_SERVER_URL = "http://control.example.test"
process.env.WORKOS_REDIRECT_URI = "http://console.example.test/auth/callback"
const { finishConnect, githubCallbackQuery } = await import("@/lib/github")
const { sampleFor } = await import("@/lib/api-sample")
const { POST } = await import("@/app/api/v1/[...path]/route")

const realFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = realFetch
})

/** The callback answers `refusal` (the one-time code is spent); the status says whether GitHub is connected. */
function server(connected: boolean, refusal = { status: 401, code: "github_oauth_failed" }) {
  const posts: string[] = []
  globalThis.fetch = (async (input: string | URL, init?: RequestInit) => {
    if (String(input).endsWith("/github/status"))
      return Response.json({ data: sampleFor("GET /v1/github/status", { connected, install_required: false }) })
    posts.push(`${init?.method} ${input}`)
    return Response.json({ error: { code: refusal.code, message: "refused" } }, { status: refusal.status })
  }) as typeof fetch
  return posts
}

describe("finishing a GitHub connect", () => {
  test.each([
    { status: 401, code: "github_oauth_failed" },
    { status: 403, code: "oauth_state_failed" },
    { status: 503, code: "unavailable" },
  ])("a repeated callback ($code) for a completed connect is a success", async (refusal) => {
    server(true, refusal)
    expect(await finishConnect("spent", "state")).toMatchObject({ connected: true })
  })

  test("a failed connect that left nothing connected stays an error", async () => {
    server(false)
    expect(finishConnect("spent", "state")).rejects.toMatchObject({ code: "github_oauth_failed" })
  })

  test("a refusal of this GitHub account is shown even with another connection", async () => {
    server(true, { status: 409, code: "github_identity_conflict" })
    expect(finishConnect("code", "state")).rejects.toMatchObject({ code: "github_identity_conflict" })
  })

  test("the one-time code is never sent again by itself", async () => {
    const posts = server(false)
    const client = new QueryClient()
    client.mount()
    const watch = () => new QueryObserver(client, githubCallbackQuery("spent", "state")).subscribe(() => {})
    const stop = watch()
    await Bun.sleep(50)
    focusManager.setFocused(false)
    focusManager.setFocused(true)
    const again = watch()
    await Bun.sleep(50)
    stop()
    again()
    client.unmount()
    focusManager.setFocused(undefined)
    expect(posts.length).toBe(1)
  })

  test("the console waits for the connect longer than for an ordinary request", async () => {
    const deadlines: number[] = []
    const timeout = AbortSignal.timeout.bind(AbortSignal)
    const spy = spyOn(AbortSignal, "timeout").mockImplementation((ms) => (deadlines.push(ms), timeout(ms)))
    globalThis.fetch = (async () => Response.json({ data: {} })) as unknown as typeof fetch
    const call = (path: string) =>
      POST(
        new Request(`http://console.example.test/api/v1/${path}`, {
          method: "POST",
          headers: { origin: "http://console.example.test", cookie: "pb-dev-session=s; pb-dev-csrf=c" },
          body: "{}",
        }),
        { params: Promise.resolve({ path: path.split("/") }) },
      )
    await call("github/oauth/callback")
    expect(deadlines).toEqual([65_000])
    deadlines.length = 0
    await call("github/oauth/start")
    expect(deadlines).toEqual([10_000])
    spy.mockRestore()
  })
})

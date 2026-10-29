import { describe, expect, test } from "bun:test"
import { ApiError, retryAfterMs } from "@/lib/api"
import { terminalCloseCodes, ticketCloseCode, workerCloseCode } from "@/lib/terminal/close-codes"

const refusal = (status: number, code: string) => new ApiError(status, code, "stub")

describe("terminal close codes", () => {
  test("a socket close names its own outcome", () => {
    expect(workerCloseCode({ state: "transport-error", code: "machine_unavailable" })).toBe("machine_unavailable")
    expect(workerCloseCode({ state: "transport-error", code: "terminal_service_unavailable" })).toBe("terminal_service_unavailable")
    expect(terminalCloseCodes.machine_unavailable.phase).toBe("reconnect")
    expect(terminalCloseCodes.machine_unavailable.message).toMatch(/machine is unavailable/)
  })
  test("a revoked close asks for a fresh ticket instead of showing 'removed' (the owner is revoked by every policy change)", () => {
    expect(terminalCloseCodes.revoked).toEqual({ phase: "reconnect", message: "", silent: true })
    expect(ticketCloseCode(refusal(403, "forbidden"))).toBe("no_access")
  })
  test("in-channel host codes map onto the table, unknown ones fall back by state", () => {
    expect(workerCloseCode({ state: "reconnecting", code: "credential_expired" })).toBe("expired")
    expect(workerCloseCode({ state: "denied", code: "something_new" })).toBe("no_access")
    expect(workerCloseCode({ state: "closed" })).toBe("ended")
    expect(workerCloseCode({ state: "reconnecting", code: "output_backpressure" })).toBeNull()
  })
  test("ticket refusals: a missing terminal has ended, it is not an access problem", () => {
    expect(ticketCloseCode(refusal(404, "not_found"))).toBe("ended")
    expect(ticketCloseCode(refusal(403, "forbidden"))).toBe("no_access")
    expect(ticketCloseCode(refusal(409, "machine_edge_unavailable"))).toBe("machine_unavailable")
    expect(ticketCloseCode(refusal(409, "browser_terminal_limit_reached"))).toBe("limit")
    expect(ticketCloseCode(refusal(0, "network"))).toBe("edge_unavailable")
    expect(ticketCloseCode(new Error("x"))).toBeNull()
  })
  test("Retry-After is read as seconds or a date, and bounded", () => {
    const at = (value: string) => new Response(null, { headers: { "retry-after": value } })
    expect(retryAfterMs(at("3"))).toBe(3000)
    expect(retryAfterMs(at("99999"))).toBe(600_000)
    expect(retryAfterMs(new Response(null), 0)).toBeUndefined()
    expect(retryAfterMs(at(new Date(5000).toUTCString()), 2000)).toBe(3000)
  })
})

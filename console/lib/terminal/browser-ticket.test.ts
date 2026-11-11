import { describe, expect, it } from "bun:test"
import { BROWSER_E2EE_SUBPROTOCOL, TICKET_PREFIX, validTicketSubprotocols } from "@/lib/terminal/browser-ticket"

describe("validTicketSubprotocols", () => {
  it("accepts the server's encrypted channel pair only", () => {
    expect(validTicketSubprotocols([BROWSER_E2EE_SUBPROTOCOL, TICKET_PREFIX + "abc"])).toBe(true)
    expect(validTicketSubprotocols(["other", TICKET_PREFIX + "abc"])).toBe(false)
    expect(validTicketSubprotocols([BROWSER_E2EE_SUBPROTOCOL, "abc"])).toBe(false)
    expect(validTicketSubprotocols([BROWSER_E2EE_SUBPROTOCOL])).toBe(false)
    expect(validTicketSubprotocols(undefined)).toBe(false)
  })
})

// The WebSocket subprotocols of an encrypted browser channel (terminal, inspector, recording): the server issues
// [BROWSER_E2EE_SUBPROTOCOL, TICKET_PREFIX + ticket] (shared/terminalprotocol on the Go side).
export const BROWSER_E2EE_SUBPROTOCOL = "paperboat.browser-terminal.e2ee.v1"
export const TICKET_PREFIX = "pb-ticket."

export function validTicketSubprotocols(subprotocols: unknown): boolean {
  return (
    Array.isArray(subprotocols) &&
    subprotocols.length === 2 &&
    subprotocols[0] === BROWSER_E2EE_SUBPROTOCOL &&
    typeof subprotocols[1] === "string" &&
    subprotocols[1].startsWith(TICKET_PREFIX)
  )
}

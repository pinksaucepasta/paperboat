import type { BrowserTerminalAPI } from "./browser-terminal-wasm"
export type RootPin = {
  version: 1
  ownerAccountID: string
  rootKeyID: string
  publicKey: string
  fingerprint: string
}

export async function verifyTicketKey(
  api: BrowserTerminalAPI,
  ownerAccountID: string,
  publicKey: string,
  fingerprint: string,
  keyID: string,
): Promise<RootPin> {
  if (!ownerAccountID || !/^[0-9a-f]{64}$/.test(fingerprint))
    throw new Error("The machine key in the terminal ticket is invalid.")
  const inspected = await api.inspectRoot(publicKey)
  if ("error" in inspected) throw new Error(inspected.message)
  const computed = inspected.fingerprint
  if (computed !== fingerprint || inspected.root_key_id !== keyID)
    throw new Error("The terminal ticket's machine key does not match its fingerprint.")
  return {
    version: 1,
    ownerAccountID,
    rootKeyID: inspected.root_key_id,
    publicKey,
    fingerprint: computed,
  }
}

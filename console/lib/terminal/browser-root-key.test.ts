import { expect, test } from "bun:test"
import { identityMatchesPin, type RootPin } from "@/lib/terminal/browser-root-key"

const pin: RootPin = { version: 1, ownerAccountID: "acct_installer", rootKeyID: "root_1", publicKey: "key", fingerprint: "a".repeat(64) }
const verified = { owner_account_id: "acct_installer", machine_id: "machine_1", root_key_id: "root_1" }

test("a member's shell on a team-owned machine matches the machine account the ticket pinned", () => {
  // The shell's owner (a member) is not the machine's account; only the pinned account counts.
  expect(identityMatchesPin(verified, "machine_1", pin)).toBe(true)
})

test("another account, machine or root key does not match", () => {
  expect(identityMatchesPin({ ...verified, owner_account_id: "acct_member" }, "machine_1", pin)).toBe(false)
  expect(identityMatchesPin(verified, "machine_2", pin)).toBe(false)
  expect(identityMatchesPin({ ...verified, root_key_id: "root_2" }, "machine_1", pin)).toBe(false)
})

import { expect, test } from "bun:test"
import { FilePasteError, parsePolicy, terminalPasteText, validateBatch } from "@/lib/terminal/file-paste"

// What the machine answers to the browser's "policy" request: its published policy. The server publishes the one Automatic
// bound; the old per-file and per-batch fields are 0 or absent.
const published = {
  policy: {
    revision: "r",
    max_file_bytes: 0,
    max_batch_files: 10,
    max_batch_bytes: 0,
    max_concurrent_transfers: 2,
    retention_seconds: 3600,
    delivery_timeout_seconds: 600,
    max_pending_spool_bytes: 1 << 30,
    automatic_max_batch_bytes: 2_000_000_000,
  },
}
const file = (size: number) => ({ size }) as File

test("the published Automatic bound is the policy; the old 50/500 MiB caps are gone", () => {
  const policy = parsePolicy(published)
  expect(policy.automatic_max_batch_bytes).toBe(2_000_000_000)
  expect(policy.max_batch_files).toBe(10)
  expect(policy.retention_seconds).toBe(3600)
  validateBatch([file(1024), file(600 * 1024 * 1024)], policy) // over the old 50 MiB file and 500 MiB batch caps
})

test("a policy without the Automatic bound is refused", () => {
  const { automatic_max_batch_bytes: _bound, ...without } = published.policy
  expect(() => parsePolicy({ policy: without })).toThrow(FilePasteError)
  expect(() => parsePolicy({ policy: { ...published.policy, automatic_max_batch_bytes: 0 } })).toThrow(FilePasteError)
})

test("a file or a batch over the Automatic bound, or too many files, is refused", () => {
  const policy = parsePolicy(published)
  expect(() => validateBatch([file(2_000_000_001)], policy)).toThrow(FilePasteError)
  expect(() => validateBatch([file(1_500_000_000), file(600_000_000)], policy)).toThrow(FilePasteError)
  expect(() => validateBatch(Array.from({ length: 11 }, () => file(1)), policy)).toThrow(FilePasteError)
})

test("pasted line breaks become one carriage return each, never a bare LF or a doubled CRLF", () => {
  expect(terminalPasteText("a\nb\r\nc")).toBe("a\rb\rc")
  expect(terminalPasteText("one line")).toBe("one line")
})

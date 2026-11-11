import { expect, test } from "bun:test"
import { shareNewFile, type InboxFile } from "@/lib/files"
import { inboxBasename } from "@/lib/terminal/inbox-upload"

const item = (id: string, name: string): InboxFile => ({ id, name, size: 1, received_at: "2026-11-11T00:00:00Z" })
const file = { name: "report.html", size: 100 } as File

/** The three calls, recording how they were used. */
function steps(options: { receipt?: string; items?: InboxFile[]; uploadFails?: string } = {}) {
  const calls = { upload: [] as string[][], shared: [] as unknown[], canceled: 0 }
  const upload = (machine: { id: string }, workspace: string, files: File[], onProgress: (p: { transferredBytes: number; totalBytes: number }) => void) => {
    calls.upload.push([machine.id, workspace, ...files.map((entry) => entry.name)])
    onProgress({ transferredBytes: 50, totalBytes: 100 })
    return {
      done: options.uploadFails ? Promise.reject(new Error(options.uploadFails)) : Promise.resolve([options.receipt ?? "/home/u/Paperboat/Inbox/report.html"]),
      cancel: () => void calls.canceled++,
    }
  }
  const list = async () => ({ total: 1, items: options.items ?? [item("file_1", "report.html")] })
  const share = async (machineId: string, fileId: string, input: { access: string; hours: number }) => {
    calls.shared.push([machineId, fileId, input])
    return "preview_1"
  }
  return { calls, steps: { upload, list, share } as unknown as NonNullable<Parameters<typeof shareNewFile>[2]> }
}
const input = { machine: { id: "machine_1" }, workspace: "personal", file, access: "private" as const, hours: 8 }

test("a new file goes into the machine's Inbox, then that Inbox file is shared with the chosen defaults", async () => {
  const { calls, steps: fake } = steps()
  const fractions: number[] = []
  const result = await shareNewFile(input, (fraction) => fractions.push(fraction), fake).done
  expect(result).toBe("preview_1")
  expect(calls.upload).toEqual([["machine_1", "personal", "report.html"]])
  expect(calls.shared).toEqual([["machine_1", "file_1", { access: "private", hours: 8 }]])
  expect(fractions).toEqual([0.5])
})

test("the Inbox file is found under the name the machine gave it, not the uploaded one", async () => {
  const { calls, steps: fake } = steps({
    receipt: "C:\\Users\\u\\Paperboat Inbox\\report (2).html",
    items: [item("file_old", "report.html"), item("file_new", "report (2).html")],
  })
  await shareNewFile(input, () => undefined, fake).done
  expect(calls.shared).toEqual([["machine_1", "file_new", { access: "private", hours: 8 }]])
})

test("a file missing from the Inbox or a failed upload is a sentence, and nothing is shared", async () => {
  const missing = steps({ items: [item("other", "other.txt")] })
  await expect(shareNewFile(input, () => undefined, missing.steps).done).rejects.toThrow(/isn't in its Inbox yet/)
  expect(missing.calls.shared).toEqual([])
  const failed = steps({ uploadFails: "Your plan's browser terminal limit is reached." })
  await expect(shareNewFile(input, () => undefined, failed.steps).done).rejects.toThrow(/browser terminal limit/)
  expect(failed.calls.shared).toEqual([])
})

test("canceling cancels the upload", () => {
  const { calls, steps: fake } = steps()
  shareNewFile(input, () => undefined, fake).cancel()
  expect(calls.canceled).toBe(1)
})

test("Inbox paths of either kind give the file name", () => {
  expect(inboxBasename("/home/u/Paperboat/Inbox/a b (2).txt")).toBe("a b (2).txt")
  expect(inboxBasename("C:\\Users\\u\\Paperboat Inbox\\a.txt")).toBe("a.txt")
  expect(inboxBasename("plain.txt")).toBe("plain.txt")
})

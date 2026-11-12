import { expect, test } from "bun:test"
import { ApiError } from "@/lib/api"
import { convertBytes, editRecordingRules, formatClock, formatDuration, parseCast, playerClock, recordingMessage, recordingRules, SegmentFramer, toBase64 } from "@/lib/recordings"

const policy = `{
  "tags": { "tag:prod": [] },
  "rules": [
    { "src": ["role:member"], "dst": ["tag:prod"], "allow": ["terminal", "record"] },
    { "src": ["role:admin"], "dst": ["machine:*"], "allow": ["recording_view"] }
  ]
}`

test("recording rules list each recording action of a rule with its index", () => {
  expect(recordingRules(policy)).toEqual([
    { index: 0, action: "record", src: ["role:member"], dst: ["tag:prod"] },
    { index: 1, action: "recording_view", src: ["role:admin"], dst: ["machine:*"] },
  ])
})

test("removing a recording action keeps the rule's other actions and drops a rule left empty", () => {
  const [record, view] = recordingRules(policy)
  const kept = JSON.parse(editRecordingRules(policy, { remove: record })!)
  expect(kept.rules[0].allow).toEqual(["terminal"])
  const dropped = JSON.parse(editRecordingRules(policy, { remove: view })!)
  expect(dropped.rules).toHaveLength(1)
  expect(dropped.tags).toEqual({ "tag:prod": [] })
})

test("adding a recording rule appends it; a file with comments is never rewritten", () => {
  const added = JSON.parse(editRecordingRules(policy, { add: { action: "record", src: "*", dst: "tag:prod" } })!)
  expect(added.rules.at(-1)).toEqual({ src: ["*"], dst: ["tag:prod"], allow: ["record"] })
  expect(editRecordingRules(`// mine\n${policy}`, { add: { action: "record", src: "*", dst: "tag:prod" } })).toBeUndefined()
})

test("an asciicast v2 recording parses into timed output and resize events", () => {
  const cast = parseCast(
    [
      JSON.stringify({ version: 2, width: 80, height: 24, timestamp: 1700000000, paperboat: { recording: "rec_x" } }),
      JSON.stringify([0.5, "o", "hello"]),
      JSON.stringify([1.25, "r", "100x30"]),
      JSON.stringify([2, "i", "typed"]),
      "",
    ].join("\n"),
  )
  expect(cast).toEqual({
    width: 80,
    height: 24,
    started: 1700000000000,
    duration: 1.25,
    events: [
      { time: 0.5, code: "o", data: "hello" },
      { time: 1.25, code: "r", data: "100x30" },
    ],
  })
  expect(() => parseCast(JSON.stringify({ version: 1, width: 80, height: 24 }))).toThrow()
})

test("durations and the player clock read naturally", () => {
  expect(formatDuration(12)).toBe("12s")
  expect(formatDuration(245)).toBe("4m 05s")
  expect(formatDuration(3725)).toBe("1h 02m")
  expect(formatClock(245)).toBe("4:05")
  expect(formatClock(3725)).toBe("1:02:05")
  // A recording of a few keystrokes lasts under a second: it still has a length, and its end reads as the end.
  expect(playerClock(0, 0.69)).toBe("0:00 / 0:01")
  expect(playerClock(0.69, 0.69)).toBe("0:01 / 0:01")
  expect(playerClock(61.2, 245.5)).toBe("1:01 / 4:06")
  expect(playerClock(0, 0)).toBe("0:00 / 0:00")
})

test("the segment framer splits length-prefixed files across frames and refuses gaps", () => {
  const file = (n: number, fill: number) => {
    const out = new Uint8Array(4 + n)
    new DataView(out.buffer).setUint32(0, n)
    out.fill(fill, 4)
    return out
  }
  const stream = new Uint8Array([...file(3, 1), ...file(5, 2)])
  const framer = new SegmentFramer()
  expect(framer.push(0, stream.slice(0, 6))).toEqual([])
  expect(framer.pending).toBe(true)
  const done = framer.push(6, stream.slice(6))
  expect(done.map((f) => Array.from(f))).toEqual([[1, 1, 1], [2, 2, 2, 2, 2]])
  expect(framer.pending).toBe(false)
  expect(() => framer.push(0, stream)).toThrow()
  expect(toBase64(new Uint8Array([255, 0, 1]))).toBe("/wAB")
})

test("byte fields convert between the server's base64 and the vault's base64url, other strings untouched", () => {
  const server = { team_id: "t+/", viewers: [{ account_id: "a", sharing_public_key: "+/8=" }], viewer_key: { generation: 1, sealed: "ab+/" } }
  const vault = convertBytes(server, "url")
  expect(vault).toEqual({ team_id: "t+/", viewers: [{ account_id: "a", sharing_public_key: "-_8" }], viewer_key: { generation: 1, sealed: "ab-_" } })
  expect(convertBytes(vault, "std")).toEqual(server)
})

test("a refused start names the machine's ownership, the usual reason recording is unavailable", () => {
  expect(recordingMessage(new ApiError(0, "recording_unavailable", ""))).toContain("must belong to the team")
})

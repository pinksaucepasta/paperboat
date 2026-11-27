import { describe, expect, test } from "bun:test"

import { HeldInput, heldInputLimit, holdsInput } from "./held-input"

const bytes = (text: string) => new TextEncoder().encode(text)
const text = (data: Uint8Array | null) => (data === null ? null : new TextDecoder().decode(data))

describe("input typed before the terminal attaches", () => {
  test("is held while connecting and reconnecting, and only then", () => {
    expect(holdsInput("connecting")).toBe(true)
    expect(holdsInput("reconnecting")).toBe(true)
    for (const phase of ["attached", "denied", "closed", "transport-error", "limit"])
      expect(holdsInput(phase)).toBe(false)
  })

  test("goes out once, in order, as one write", () => {
    const held = new HeldInput()
    for (const key of "echo hi8000\r") expect(held.add(bytes(key))).toBe(true)
    expect(text(held.take())).toBe("echo hi8000\r")
    expect(held.take()).toBeNull()
  })

  test("keeps its own copy of each chunk", () => {
    const held = new HeldInput()
    const chunk = bytes("a")
    held.add(chunk)
    chunk[0] = 0x62
    expect(text(held.take())).toBe("a")
  })

  test("past the limit takes nothing more, so no later Enter runs a line with a hole in it", () => {
    const held = new HeldInput()
    expect(held.add(bytes("rm -r "))).toBe(true)
    expect(held.add(new Uint8Array(heldInputLimit))).toBe(false)
    expect(held.add(bytes("\r"))).toBe(false)
    expect(held.cut).toBe(true)
    expect(text(held.take())).toBe("rm -r ")
    expect(held.cut).toBe(false)
    expect(held.add(bytes("ls\r"))).toBe(true)
    expect(text(held.take())).toBe("ls\r")
  })

  test("takes exactly the limit", () => {
    const held = new HeldInput()
    expect(held.add(new Uint8Array(heldInputLimit))).toBe(true)
    expect(held.take()?.byteLength).toBe(heldInputLimit)
  })
})

import { expect, test } from "bun:test"
import { bodyText } from "./body"

test("text passes through", () => expect(bodyText("a\r\nb").shown).toBe("a\r\nb"))

test("base64 shows every byte as captured", () => {
  const b = bodyText(btoa(String.fromCharCode(0x89, 0x50, 0x4e, 0x47, 0, 0xff, 0x63, 0x61, 0x66, 0xe9)), "base64")
  expect(b.shown).toBe("00000000  89 50 4e 47 00 ff 63 61 66 e9                    .PNG..caf.")
  expect(b.note).toContain("10 bytes")
})

test("long base64 is cut and says so", () => {
  const b = bodyText(btoa("x".repeat(5000)), "base64")
  expect(b.shown.split("\n").length).toBe(256)
  expect(b.note).toContain("5000 bytes, first 4096 shown")
})

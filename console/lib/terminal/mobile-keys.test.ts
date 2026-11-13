import { expect, test } from "bun:test"
import { optionKeySequence } from "@/lib/terminal/mobile-keys"

const key = (k: string, code: string, mods: Partial<Record<"altKey" | "ctrlKey" | "metaKey" | "shiftKey", boolean>> = {}) => ({
  key: k,
  code,
  altKey: false,
  ctrlKey: false,
  metaKey: false,
  shiftKey: false,
  ...mods,
})

test("Option+Left and Option+Right are word motion on macOS only", () => {
  expect(optionKeySequence(key("ArrowLeft", "ArrowLeft", { altKey: true }), true)).toBe("\x1bb")
  expect(optionKeySequence(key("ArrowRight", "ArrowRight", { altKey: true }), true)).toBe("\x1bf")
  expect(optionKeySequence(key("ArrowLeft", "ArrowLeft", { altKey: true }), false)).toBeNull()
  expect(optionKeySequence(key("ArrowLeft", "ArrowLeft"), true)).toBeNull()
  expect(optionKeySequence(key("ArrowLeft", "ArrowLeft", { altKey: true, shiftKey: true }), true)).toBeNull()
  expect(optionKeySequence(key("ArrowUp", "ArrowUp", { altKey: true }), true)).toBeNull()
})

test("Safari reports Option+arrow with the control character as key; code still says which arrow", () => {
  expect(optionKeySequence(key("\u001c", "ArrowLeft", { altKey: true }), true)).toBe("\x1bb")
  expect(optionKeySequence(key("\u001d", "ArrowRight", { altKey: true }), true)).toBe("\x1bf")
  expect(optionKeySequence(key("\u001e", "ArrowUp", { altKey: true }), true)).toBeNull()
})

test("Option+letter sends the composed character without ESC on macOS", () => {
  expect(optionKeySequence(key("\u00b4", "KeyE", { altKey: true }), true)).toBe("\u00b4")
  expect(optionKeySequence(key("\u00a9", "KeyG", { altKey: true }), true)).toBe("\u00a9")
  expect(optionKeySequence(key("\u00b4", "KeyE", { altKey: true }), false)).toBeNull()
  expect(optionKeySequence(key("b", "KeyB", { altKey: true }), true)).toBeNull()
  expect(optionKeySequence(key("Dead", "KeyE", { altKey: true }), true)).toBeNull()
})

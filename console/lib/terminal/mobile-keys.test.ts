import { expect, test } from "bun:test"
import { optionArrowSequence } from "@/lib/terminal/mobile-keys"

const key = (k: string, mods: Partial<Record<"altKey" | "ctrlKey" | "metaKey" | "shiftKey", boolean>> = {}) => ({
  key: k,
  altKey: false,
  ctrlKey: false,
  metaKey: false,
  shiftKey: false,
  ...mods,
})

test("Option+Left and Option+Right are word motion on macOS only", () => {
  expect(optionArrowSequence(key("ArrowLeft", { altKey: true }), true)).toBe("\x1bb")
  expect(optionArrowSequence(key("ArrowRight", { altKey: true }), true)).toBe("\x1bf")
  expect(optionArrowSequence(key("ArrowLeft", { altKey: true }), false)).toBeNull()
  expect(optionArrowSequence(key("ArrowLeft"), true)).toBeNull()
  expect(optionArrowSequence(key("ArrowLeft", { altKey: true, shiftKey: true }), true)).toBeNull()
  expect(optionArrowSequence(key("ArrowUp", { altKey: true }), true)).toBeNull()
})

export type MobileKey = "ESC" | "/" | "-" | "HOME" | "UP" | "END" | "PGUP" | "TAB" | "LEFT" | "DOWN" | "RIGHT" | "PGDN"

export type MobileModifiers = { ctrl: boolean; alt: boolean }

export const noMobileModifiers: MobileModifiers = { ctrl: false, alt: false }

const arrowFinal: Partial<Record<MobileKey, string>> = { UP: "A", DOWN: "B", RIGHT: "C", LEFT: "D" }

function modifierParameter(modifiers: MobileModifiers): number {
  return 1 + (modifiers.alt ? 2 : 0) + (modifiers.ctrl ? 4 : 0)
}

function controlCharacter(character: string): string | null {
  if (/^[a-z]$/i.test(character)) return String.fromCharCode(character.toUpperCase().charCodeAt(0) - 64)
  const codes: Record<string, number> = {
    " ": 0,
    "@": 0,
    "[": 27,
    "\\": 28,
    "]": 29,
    "^": 30,
    _: 31,
    "/": 31,
    "-": 31,
    "?": 127,
  }
  return character in codes ? String.fromCharCode(codes[character]) : null
}

export function applyMobileModifiers(input: string, modifiers: MobileModifiers): string {
  if (!input || (!modifiers.ctrl && !modifiers.alt)) return input
  let output = input
  if (modifiers.ctrl) {
    const first = Array.from(input)[0]
    const control = controlCharacter(first)
    if (control !== null) output = control + input.slice(first.length)
  }
  return modifiers.alt ? `\x1b${output}` : output
}

export function mobileKeySequence(key: MobileKey, applicationCursorMode: boolean, modifiers: MobileModifiers): string {
  const parameter = modifierParameter(modifiers)
  const arrow = arrowFinal[key]
  if (arrow)
    return parameter > 1 ? `\x1b[1;${parameter}${arrow}` : applicationCursorMode ? `\x1bO${arrow}` : `\x1b[${arrow}`
  if (key === "HOME" || key === "END") {
    const final = key === "HOME" ? "H" : "F"
    return parameter > 1 ? `\x1b[1;${parameter}${final}` : applicationCursorMode ? `\x1bO${final}` : `\x1b[${final}`
  }
  if (key === "PGUP" || key === "PGDN")
    return `\x1b[${key === "PGUP" ? "5" : "6"}${parameter > 1 ? `;${parameter}` : ""}~`
  if (key === "ESC") return modifiers.alt ? "\x1b\x1b" : "\x1b"
  if (key === "TAB") return modifiers.alt ? "\x1b\t" : "\t"
  return applyMobileModifiers(key, modifiers)
}

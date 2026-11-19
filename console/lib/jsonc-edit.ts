// Edits a HuJSON file (JSON with comments and trailing commas) as text, so the GUI editors write the same
// file the file editor shows: everything outside the value being changed (comments, keys the GUI does not
// know, the order and layout of other entries) stays byte for byte. Comments inside a replaced value are lost.

export type Node = { start: number; end: number } & (
  | { kind: "object"; members: Member[] }
  | { kind: "array"; items: Node[] }
  | { kind: "scalar" }
)
export type Member = { key: string; keyStart: number; value: Node }

/** The file's syntax tree with the offsets of every value, or undefined when it is not valid HuJSON. */
export function parseTree(text: string): Node | undefined {
  let i = 0
  const fail = () => {
    throw new SyntaxError("hujson")
  }
  const skip = () => {
    for (;;) {
      while (i < text.length && /\s/.test(text[i])) i++
      if (text.startsWith("//", i)) while (i < text.length && text[i] !== "\n") i++
      else if (text.startsWith("/*", i)) {
        const end = text.indexOf("*/", i + 2)
        if (end < 0) fail()
        i = end + 2
      } else return
    }
  }
  const string = () => {
    const start = i
    for (i++; i < text.length && text[i] !== '"'; i++) if (text[i] === "\\") i++
    if (i >= text.length) fail()
    i++
    return text.slice(start, i)
  }
  const value = (): Node => {
    skip()
    const start = i
    const c = text[i]
    if (c === "{") {
      const members: Member[] = []
      for (i++; ; ) {
        skip()
        if (text[i] === "}") break
        if (text[i] !== '"') fail()
        const keyStart = i
        const key = JSON.parse(string()) as string
        skip()
        if (text[i++] !== ":") fail()
        members.push({ key, keyStart, value: value() })
        skip()
        if (text[i] === ",") i++
        else if (text[i] !== "}") fail()
      }
      i++
      return { kind: "object", start, end: i, members }
    }
    if (c === "[") {
      const items: Node[] = []
      for (i++; ; ) {
        skip()
        if (text[i] === "]") break
        items.push(value())
        skip()
        if (text[i] === ",") i++
        else if (text[i] !== "]") fail()
      }
      i++
      return { kind: "array", start, end: i, items }
    }
    if (c === '"') string()
    else {
      while (i < text.length && !/[\s,\]}]/.test(text[i]) && !text.startsWith("//", i) && !text.startsWith("/*", i)) i++
      if (i === start) fail()
    }
    return { kind: "scalar", start, end: i }
  }
  try {
    const root = value()
    skip()
    return i === text.length ? root : undefined
  } catch {
    return undefined
  }
}

/** The text of a value, parsed (comments dropped). */
export const valueOf = (text: string, node: Node): unknown => JSON.parse(standard(text.slice(node.start, node.end)))

function standard(text: string): string {
  const tree = parseTree(text)
  if (!tree) throw new SyntaxError("hujson")
  let out = ""
  const walk = (n: Node) => {
    if (n.kind === "scalar") out += text.slice(n.start, n.end)
    else if (n.kind === "array") {
      out += "["
      n.items.forEach((item, k) => (k && (out += ","), walk(item)))
      out += "]"
    } else {
      out += "{"
      n.members.forEach((m, k) => ((k && (out += ",")), (out += JSON.stringify(m.key) + ":"), walk(m.value)))
      out += "}"
    }
  }
  walk(tree)
  return out
}

export const member = (root: Node, key: string) => (root.kind === "object" ? root.members.find((m) => m.key === key) : undefined)

const indentOf = (text: string, at: number) => /^[ \t]*/.exec(text.slice(text.lastIndexOf("\n", at - 1) + 1))![0]

/** JSON for `value`, its lines after the first indented by `indent`. */
export const json = (value: unknown, indent: string) => JSON.stringify(value, null, 2).replaceAll("\n", "\n" + indent)

const splice = (text: string, start: number, end: number, replacement: string) => text.slice(0, start) + replacement + text.slice(end)

/** Where a member or item is removed from: its own line when it has the line to itself, and its comma. */
function removal(text: string, start: number, end: number): [number, number] {
  let to = end
  const after = /^[ \t]*,?[ \t]*(\r?\n)?/.exec(text.slice(end))![0]
  to += after.length
  let from = start
  const before = text.slice(text.lastIndexOf("\n", start - 1) + 1, start)
  if (/^[ \t]*$/.test(before) && after.endsWith("\n")) from = start - before.length
  else if (/^[ \t]*$/.test(before) && after.trim() === "") from = start - before.length
  return [from, to]
}

/** Sets a top-level key to `value`, or removes it when `value` is undefined. Returns the new text. */
export function setMember(text: string, key: string, value: unknown): string {
  const root = parseTree(text)
  if (!root || root.kind !== "object") return text
  const found = member(root, key)
  if (found) {
    if (value === undefined) {
      const [from, to] = removal(text, found.keyStart, found.value.end)
      return splice(text, from, to, "")
    }
    return splice(text, found.value.start, found.value.end, json(value, indentOf(text, found.keyStart)))
  }
  if (value === undefined) return text
  const last = root.members.at(-1)
  const indent = last ? indentOf(text, last.keyStart) : "  "
  const entry = `${JSON.stringify(key)}: ${json(value, indent)}`
  if (last) return splice(text, last.value.end, last.value.end, `,\n${indent}${entry}`)
  return splice(text, root.start + 1, root.end - 1, `\n${indent}${entry}\n`)
}

/** Replaces item `index` of the top-level array `key` (index === length appends; `undefined` removes). */
export function setItem(text: string, key: string, index: number, value: unknown): string {
  const root = parseTree(text)
  if (!root || root.kind !== "object") return text
  const array = member(root, key)?.value
  if (!array) return value === undefined ? text : setMember(text, key, [value])
  if (array.kind !== "array") return text
  const item = array.items[index]
  if (item) {
    if (value === undefined) {
      if (array.items.length === 1) return setMember(text, key, undefined)
      const [from, to] = removal(text, item.start, item.end)
      return splice(text, from, to, "")
    }
    return splice(text, item.start, item.end, json(value, indentOf(text, item.start)))
  }
  if (value === undefined) return text
  const last = array.items.at(-1)
  if (!last) return splice(text, array.start + 1, array.end - 1, `\n    ${json(value, "    ")}\n  `)
  const indent = indentOf(text, last.start)
  return splice(text, last.end, last.end, `,\n${indent}${json(value, indent)}`)
}
